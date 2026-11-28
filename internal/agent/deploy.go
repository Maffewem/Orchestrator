package agent

import (
	"context"
	"errors"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Maffewem/Orchestrator/internal/api"
)

var credentialsRe = regexp.MustCompile(`://[^/@\s]+@`)

// redact strips credentials from URLs (build remotes carry a GitHub token).
func redact(s string) string { return credentialsRe.ReplaceAllString(s, "://***@") }

// steps streams a deployment's step progress and logs to the control plane.
type steps struct {
	a    *Agent
	id   string
	mu   sync.Mutex // serialises posts so log order is preserved
	cur  int
	buf  []api.LogLine
	stop chan struct{}
	done chan struct{}
}

func (a *Agent) newSteps(ctx context.Context, deploymentID string) *steps {
	s := &steps{a: a, id: deploymentID, cur: -1, stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(s.done)
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-s.stop:
				return
			case <-t.C:
				s.flush(ctx, "")
			}
		}
	}()
	return s
}

func (s *steps) logf(format string, args ...any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.buf = append(s.buf, api.LogLine{Time: time.Now(), Text: redact(fmt.Sprintf(format, args...))})
}

func (s *steps) flush(ctx context.Context, status string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cur < 0 || (status == "" && len(s.buf) == 0) {
		return
	}
	u := api.StepUpdate{Status: status, Lines: s.buf}
	s.buf = nil
	path := "/api/agent/deployments/" + s.id + "/steps/" + strconv.Itoa(s.cur)
	if err := s.a.post(ctx, path, s.a.state.AgentToken, u, nil); err != nil {
		s.a.log.Warn("step update failed", "deployment", s.id, "err", err)
	}
}

// begin ends the current step successfully (if any) and starts step idx.
func (s *steps) begin(ctx context.Context, idx int) {
	if s.cur >= 0 {
		s.flush(ctx, api.StepSucceeded)
	}
	s.mu.Lock()
	s.cur = idx
	s.mu.Unlock()
	s.flush(ctx, api.StepRunning)
}

func (s *steps) finish(ctx context.Context, err error) {
	close(s.stop)
	<-s.done
	if err != nil {
		s.logf("Error: %v", err)
		s.flush(ctx, api.StepFailed)
		return
	}
	s.logf("Success: deployment complete")
	s.flush(ctx, api.StepSucceeded)
}

// deploy rolls spec out with a simple blue/green swap: the previous container is renamed
// and stopped, the new one started and verified, and the previous one restored on failure.
func (a *Agent) deploy(ctx context.Context, deploymentID string, spec api.DeploySpec) (err error) {
	st := a.newSteps(ctx, deploymentID)
	defer func() { st.finish(context.WithoutCancel(ctx), err) }()

	if spec.Compose != nil {
		return a.deployCompose(ctx, st, deploymentID, spec)
	}
	st.begin(ctx, api.StepIndex(spec, api.StepInit))
	st.logf("Initialising deployment %s on %s", deploymentID, a.hostInfo().Hostname)
	st.logf("Service: %s", spec.Service)
	st.logf("Image: %s", spec.Image)
	if len(spec.Ports) > 0 {
		st.logf("Ports: %s", strings.Join(spec.Ports, ", "))
	}
	st.logf("Restart policy: %s", spec.Restart)
	info := a.docker.Info(ctx)
	if info.Error != "" {
		return fmt.Errorf("docker engine unavailable: %s", info.Error)
	}
	st.logf("Docker engine %s ready", info.Version)
	var att *attachment
	if spec.Dedicated != nil {
		if att, err = a.prepareDedicated(ctx, st, spec); err != nil {
			return err
		}
	} else if err := a.checkPorts(ctx, st, spec); err != nil {
		return err
	}

	reuse := spec.Build != nil && spec.Reuse && a.docker.hasImage(ctx, spec.Image)
	switch {
	case api.IsNode(spec) && reuse:
		st.begin(ctx, api.StepIndex(spec, api.StepCloning))
		st.logf("%s is already on this device; skipping the clone and build", spec.Image)
	case api.IsNode(spec):
		if err := a.buildNode(ctx, st, spec); err != nil {
			return fmt.Errorf("build failed: %w", err)
		}
	case reuse:
		st.begin(ctx, api.StepIndex(spec, api.StepBuilding))
		st.logf("%s is already on this device; skipping the build", spec.Image)
	case spec.Build != nil:
		st.begin(ctx, api.StepIndex(spec, api.StepBuilding))
		st.logf("Cloning repository and building %s on this device…", spec.Image)
		if err := a.docker.Build(ctx, spec.Build.Remote, spec.Build.Dockerfile, spec.Image, func(l string) { st.logf("%s", l) }); err != nil {
			if strings.Contains(err.Error(), "git") {
				return fmt.Errorf("build failed: %w (the device needs git installed for Docker to clone the repository)", err)
			}
			return fmt.Errorf("build failed: %w", err)
		}
		st.logf("Success: built %s", spec.Image)
	default:
		st.begin(ctx, api.StepIndex(spec, api.StepPulling))
		st.logf("Pulling %s…", spec.Image)
		if err := a.docker.Pull(ctx, spec.Image, func(l string) { st.logf("%s", l) }); err != nil {
			return fmt.Errorf("pull failed: %w", err)
		}
		st.logf("Success: image is up to date")
		if dg := a.pulledDigest(ctx, spec.Image); dg != "" {
			st.logf("Image digest: %s", dg)
			a.digests.Store(deploymentID, dg)
		}
	}

	st.begin(ctx, api.StepIndex(spec, api.StepDeploy))
	// The build can take minutes, so look again before touching the running version.
	if spec.Dedicated == nil {
		if err := a.checkPorts(ctx, st, spec); err != nil {
			return err
		}
	}
	previous, err := a.docker.ContainerByName(ctx, spec.Service)
	if err != nil {
		return err
	}
	backup := fmt.Sprintf("%s-previous-%d", spec.Service, time.Now().Unix())
	if previous != "" {
		st.logf("Stopping previous version (%.12s)", previous)
		if err := a.docker.Rename(ctx, previous, backup); err != nil {
			return fmt.Errorf("rename previous container: %w", err)
		}
		if err := a.docker.Stop(ctx, previous); err != nil {
			a.restore(ctx, st, previous, spec.Service)
			return fmt.Errorf("stop previous container: %w", err)
		}
	}
	labels := map[string]string{"orchestrator.deployment": deploymentID, "orchestrator.service": spec.Service}
	var prevIP, prevDedicated, prevParent string
	if previous != "" {
		prevParent = a.docker.containerParentOn(ctx, previous)
		prevDedicated, _ = a.docker.LabelOf(ctx, previous, labelDedicated)
		prevIP = a.docker.containerIPOn(ctx, previous, macvlanPrefix)
	}
	if att != nil {
		labels[labelDedicated], labels[labelDedMAC] = spec.Dedicated.Mode, att.MAC.String()
		if spec.Dedicated.HostIP != "" {
			labels[labelDedHost] = spec.Dedicated.HostIP
		}
	}
	id, err := a.docker.CreateOn(ctx, spec.Service, spec, labels, att)
	if err != nil {
		a.restore(ctx, st, previous, spec.Service)
		return fmt.Errorf("create container: %w", err)
	}
	st.logf("Created container %.12s", id)
	if err := a.docker.Start(ctx, id); err != nil {
		a.discard(ctx, id)
		a.restore(ctx, st, previous, spec.Service)
		return fmt.Errorf("start container: %w", portStartError(err))
	}
	st.logf("Started container %.12s", id)

	if att != nil && spec.Dedicated.HostIP != "" {
		if err := ensureShim(ctx, att.LAN.Parent, spec.Dedicated.HostIP, att.IP); err != nil {
			st.logf("Warning: the device can't reach the worker's address: %v", err)
		} else {
			st.logf("This device reaches the worker at %s through %s (own address %s)", att.IP, shimFor(att.LAN.Parent), spec.Dedicated.HostIP)
		}
	}
	st.begin(ctx, api.StepIndex(spec, api.StepVerify))
	st.logf("Waiting for the container to stay up…")
	state, err := a.docker.waitRunning(ctx, id, 8*time.Second)
	if err == nil && (!state.Running || state.Restarting) {
		if logs, lerr := a.docker.Logs(ctx, id, 40); lerr == nil && logs != "" {
			st.logf("Last output from the container:")
			for _, l := range strings.Split(strings.TrimRight(logs, "\n"), "\n") {
				st.logf("  %s", l)
			}
		}
		err = fmt.Errorf("container is %s (exit code %d)", state.Status, state.ExitCode)
	}
	if err != nil {
		a.discard(ctx, id)
		a.restore(ctx, st, previous, spec.Service)
		return err
	}
	st.logf("Container is %s", state.Status)
	probe := spec
	if att != nil {
		// Published ports don't exist here: probe the container ports on its own address, which the
		// device can only reach through the shim.
		probe.Ports = nil
		if spec.Dedicated.HostIP != "" {
			for _, m := range spec.Ports {
				_, c, ok := strings.Cut(m, ":")
				if !ok {
					c = m
				}
				probe.Ports = append(probe.Ports, "1:"+c)
			}
		} else if len(spec.Ports) > 0 {
			st.logf("Skipping the port check: this device can't reach a worker's own address without an address for this device to reach the worker")
		}
	}
	if rerr := a.waitReachable(ctx, st, id, probe, 40*time.Second); rerr != nil {
		if logs, lerr := a.docker.Logs(ctx, id, 40); lerr == nil && logs != "" {
			st.logf("Last output from the container:")
			for _, l := range strings.Split(strings.TrimRight(logs, "\n"), "\n") {
				st.logf("  %s", l)
			}
		}
		if api.IsNode(spec) || spec.Labels["orchestrator.preview"] != "" {
			a.discard(ctx, id)
			a.restore(ctx, st, previous, spec.Service)
			return rerr
		}
		st.logf("Warning: %v", rerr)
	}
	switch {
	case att != nil && spec.Dedicated.Mode == api.DedicatedDHCP && att.Lease != nil:
		a.holdLease(spec.Service, att.LAN.Parent, att.MAC, att.Lease)
	case att == nil && prevDedicated != "":
		a.dropLease(spec.Service)
	}
	if prevIP != "" && (att == nil || !att.IP.Equal(net.ParseIP(prevIP))) {
		dropShimRoute(ctx, prevParent, prevIP)
	}
	if previous != "" {
		if err := a.docker.Remove(ctx, previous); err != nil {
			st.logf("Warning: could not remove previous container %s: %v", backup, err)
		} else {
			st.logf("Removed previous version")
		}
	}
	if prevParent != "" && (att == nil || att.LAN.Parent != prevParent) {
		a.releaseParent(ctx, prevParent) // the worker left this network or VLAN
	}
	a.trimOwnImages(ctx, st, spec)
	return nil
}

// discard removes a container from a failed deployment.
func (a *Agent) discard(ctx context.Context, id string) {
	if err := a.docker.Remove(context.WithoutCancel(ctx), id); err != nil {
		a.log.Warn("remove failed container", "id", id, "err", err)
	}
}

// restore brings the previous version back after a failed deployment.
func (a *Agent) restore(ctx context.Context, st *steps, previous, name string) {
	if previous == "" {
		return
	}
	ctx = context.WithoutCancel(ctx)
	err := errors.Join(a.docker.Rename(ctx, previous, name), a.docker.Start(ctx, previous))
	if err != nil {
		st.logf("Rollback failed: %v", err)
		return
	}
	st.logf("Rolled back: previous version is running again")
}

// checkPorts fails the deployment early when a requested host port is taken.
func (a *Agent) checkPorts(ctx context.Context, st *steps, spec api.DeploySpec) error {
	if len(spec.Ports) == 0 {
		return nil
	}
	cs, err := a.docker.Containers(ctx)
	if err != nil {
		st.logf("Warning: could not check for port conflicts: %v", err)
		return nil
	}
	bad := portConflicts(spec, cs, portBusy)
	if len(bad) == 0 {
		st.logf("Host ports are free")
		return nil
	}
	for _, b := range bad {
		st.logf("Error: %s", b)
	}
	return fmt.Errorf("port conflict: %s", bad[0])
}

// trimOwnImages removes this worker's older built images beyond spec.KeepImages. It is best effort:
// a failure never fails the deployment, and an image still in use is never removed.
func (a *Agent) trimOwnImages(ctx context.Context, st *steps, spec api.DeploySpec) {
	if spec.KeepImages <= 0 || spec.Build == nil {
		return
	}
	repo, _ := splitImage(spec.Image)
	if !strings.HasPrefix(repo, gcRepoPrefix) {
		return
	}
	res, err := a.docker.Prune(context.WithoutCancel(ctx), PruneOptions{Keep: spec.KeepImages, Trim: true, Only: repo})
	switch {
	case err != nil:
		st.logf("Warning: could not trim old images: %v", err)
	case res.ImagesRemoved > 0:
		st.logf("Removed %d older image(s) of %s, keeping the newest %d", res.ImagesRemoved, spec.Service, spec.KeepImages)
	}
}

// pulledDigest returns the sha256 digest the engine resolved ref to, or "" when it has none
// (an image that was only built locally has no registry digest).
func (a *Agent) pulledDigest(ctx context.Context, ref string) string {
	info, err := a.docker.ImageInspect(ctx, ref)
	if err != nil {
		return ""
	}
	repo, _ := splitImage(ref)
	if i := strings.Index(repo, "@"); i >= 0 {
		repo = repo[:i]
	}
	for _, rd := range info.RepoDigests {
		if r, dg, ok := strings.Cut(rd, "@"); ok && r == repo && strings.HasPrefix(dg, "sha256:") {
			return dg
		}
	}
	return ""
}
