package agent

import (
	"context"
	"errors"
	"fmt"
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

	st.begin(ctx, api.StepInit)
	st.logf("Initializing deployment %s on %s", deploymentID, a.hostInfo().Hostname)
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

	st.begin(ctx, api.StepSource)
	if spec.Build != nil && spec.Reuse && a.docker.hasImage(ctx, spec.Image) {
		st.logf("%s is already on this device; skipping the build", spec.Image)
	} else if spec.Build != nil {
		st.logf("Cloning repository and building %s on this device...", spec.Image)
		if err := a.docker.Build(ctx, spec.Build.Remote, spec.Build.Dockerfile, spec.Image, func(l string) { st.logf("%s", l) }); err != nil {
			if strings.Contains(err.Error(), "git") {
				return fmt.Errorf("build failed: %w (the device needs git installed for Docker to clone the repository)", err)
			}
			return fmt.Errorf("build failed: %w", err)
		}
		st.logf("Success: built %s", spec.Image)
	} else {
		st.logf("Pulling %s...", spec.Image)
		if err := a.docker.Pull(ctx, spec.Image, func(l string) { st.logf("%s", l) }); err != nil {
			return fmt.Errorf("pull failed: %w", err)
		}
		st.logf("Success: image is up to date")
	}

	st.begin(ctx, api.StepDeploy)
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
	id, err := a.docker.Create(ctx, spec.Service, spec, labels)
	if err != nil {
		a.restore(ctx, st, previous, spec.Service)
		return fmt.Errorf("create container: %w", err)
	}
	st.logf("Created container %.12s", id)
	if err := a.docker.Start(ctx, id); err != nil {
		a.discard(ctx, id)
		a.restore(ctx, st, previous, spec.Service)
		return fmt.Errorf("start container: %w", err)
	}
	st.logf("Started container %.12s", id)

	st.begin(ctx, api.StepVerify)
	st.logf("Waiting for the container to stay up...")
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
	if previous != "" {
		if err := a.docker.Remove(ctx, previous); err != nil {
			st.logf("Warning: could not remove previous container %s: %v", backup, err)
		} else {
			st.logf("Removed previous version")
		}
	}
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
