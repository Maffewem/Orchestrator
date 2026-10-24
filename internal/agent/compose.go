package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/Maffewem/Orchestrator/internal/api"
)

var envNameOK = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// stackDir is where a stack's compose file and .env live on the device.
func (a *Agent) stackDir(name string) string {
	return filepath.Join(filepath.Dir(a.cfg.StatePath), "stacks", name)
}

// dotenv renders KEY=value pairs as a Compose env file. Values are single-quoted so $ and #
// stay literal; a value holding a single quote falls back to double quotes with escapes.
func dotenv(env []string) (string, error) {
	var b strings.Builder
	for _, e := range env {
		k, v, ok := strings.Cut(e, "=")
		if !ok || !envNameOK.MatchString(k) {
			return "", fmt.Errorf("environment variable %q is not KEY=value", e)
		}
		if strings.ContainsAny(v, "\n\r") {
			return "", fmt.Errorf("%s: values with line breaks are not supported in stacks", k)
		}
		if strings.Contains(v, "'") {
			v = strings.NewReplacer(`\`, `\\`, `"`, `\"`, `$`, `\$`).Replace(v)
			fmt.Fprintf(&b, "%s=\"%s\"\n", k, v)
		} else {
			fmt.Fprintf(&b, "%s='%s'\n", k, v)
		}
	}
	return b.String(), nil
}

// composeCmd builds a `docker compose` invocation for a stack that has been written to disk.
func (a *Agent) composeCmd(ctx context.Context, name string, args ...string) (*exec.Cmd, error) {
	cli, err := exec.LookPath("docker")
	if err != nil {
		return nil, errors.New("the docker CLI is required on this device to run Compose stacks")
	}
	dir := a.stackDir(name)
	full := append([]string{"compose", "-p", name, "-f", filepath.Join(dir, "compose.yml"), "--env-file", filepath.Join(dir, ".env")}, args...)
	cmd := exec.CommandContext(ctx, cli, full...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "DOCKER_HOST="+a.docker.host)
	return cmd, nil
}

func (a *Agent) stackContainers(ctx context.Context, name string) ([]dockerContainer, error) {
	return a.docker.listContainers(ctx, map[string][]string{"label": {"com.docker.compose.project=" + name}})
}

// deployCompose writes the stack's files and brings it up with `docker compose up -d`. Unlike
// a single container there is no blue/green swap: Compose recreates only the services that changed.
func (a *Agent) deployCompose(ctx context.Context, st *steps, deploymentID string, spec api.DeploySpec) error {
	st.begin(ctx, api.StepIndex(spec, api.StepInit))
	st.logf("Initializing deployment %s on %s", deploymentID, a.hostInfo().Hostname)
	st.logf("Stack: %s", spec.Service)
	info := a.docker.Info(ctx)
	if info.Error != "" {
		return fmt.Errorf("docker engine unavailable: %s", info.Error)
	}
	st.logf("Docker engine %s ready", info.Version)
	env, err := dotenv(spec.Compose.Env)
	if err != nil {
		return err
	}
	dir := a.stackDir(spec.Service)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "compose.yml"), []byte(spec.Compose.YAML), 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(env), 0o600); err != nil {
		return err
	}
	st.logf("Wrote compose.yml and %d environment value(s) to %s", len(spec.Compose.Env), dir)
	cmd, err := a.composeCmd(ctx, spec.Service, "config", "--quiet")
	if err != nil {
		return err
	}
	if err := runStream(cmd, func(l string) { st.logf("%s", l) }); err != nil {
		return fmt.Errorf("the compose file is not valid: %w", err)
	}
	st.logf("Compose file is valid")

	st.begin(ctx, api.StepIndex(spec, api.StepPulling))
	if cmd, err = a.composeCmd(ctx, spec.Service, "pull", "--ignore-pull-failures"); err != nil {
		return err
	}
	if err := runStream(cmd, func(l string) { st.logf("%s", l) }); err != nil {
		return fmt.Errorf("pull failed: %w", err)
	}

	st.begin(ctx, api.StepIndex(spec, api.StepDeploy))
	if cmd, err = a.composeCmd(ctx, spec.Service, "up", "-d", "--remove-orphans"); err != nil {
		return err
	}
	if err := runStream(cmd, func(l string) { st.logf("%s", l) }); err != nil {
		return fmt.Errorf("compose up failed: %w", err)
	}

	st.begin(ctx, api.StepIndex(spec, api.StepVerify))
	st.logf("Waiting for the services to stay up...")
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(8 * time.Second):
	}
	cs, err := a.stackContainers(ctx, spec.Service)
	if err != nil {
		return err
	}
	if len(cs) == 0 {
		return errors.New("compose started no containers")
	}
	var bad []string
	for _, c := range cs {
		state, _, err := a.docker.Inspect(ctx, c.ID)
		if err != nil {
			return err
		}
		name := strings.TrimPrefix(firstName(c.Names), "/")
		switch {
		case state.Restarting:
			bad = append(bad, name+" is restarting")
		case !state.Running && state.ExitCode != 0:
			bad = append(bad, fmt.Sprintf("%s exited with code %d", name, state.ExitCode))
		case state.Running:
			st.logf("%s is running", name)
		default:
			st.logf("%s finished (exit code 0)", name) // one-shot services such as migrations
		}
		if !state.Running && state.ExitCode != 0 || state.Restarting {
			if logs, lerr := a.docker.Logs(ctx, c.ID, 30); lerr == nil && logs != "" {
				st.logf("Last output from %s:", name)
				for _, l := range strings.Split(strings.TrimRight(logs, "\n"), "\n") {
					st.logf("  %s", l)
				}
			}
		}
	}
	if len(bad) > 0 {
		return errors.New(strings.Join(bad, "; "))
	}
	return nil
}

func firstName(names []string) string {
	if len(names) == 0 {
		return ""
	}
	return names[0]
}

// stackAction starts, stops or removes every container of a stack. Start and stop go through
// the Engine API so they work even if the compose file is gone; down uses the compose CLI when
// the stack's files are still on the device (it also removes networks) and otherwise removes the containers.
func (a *Agent) stackAction(ctx context.Context, action, name, volumes string) (string, error) {
	if name == "" || strings.ContainsAny(name, "/\\") {
		return "", errors.New("stack name required")
	}
	if action == api.ActionStackDown {
		if _, err := os.Stat(filepath.Join(a.stackDir(name), "compose.yml")); err == nil {
			args := []string{"down", "--remove-orphans"}
			if volumes == "delete" {
				args = append(args, "--volumes")
			}
			cmd, err := a.composeCmd(ctx, name, args...)
			if err == nil {
				var lines []string
				if err = runStream(cmd, func(l string) { lines = append(lines, l) }); err == nil {
					os.RemoveAll(a.stackDir(name))
					return strings.Join(lines, "\n"), nil
				}
				return strings.Join(lines, "\n"), err
			}
		}
	}
	cs, err := a.stackContainers(ctx, name)
	if err != nil {
		return "", err
	}
	if len(cs) == 0 && action != api.ActionStackDown {
		return "", fmt.Errorf("no containers found for stack %s", name)
	}
	var errs []error
	for _, c := range cs {
		switch action {
		case api.ActionStackStart:
			errs = append(errs, a.docker.Start(ctx, c.ID))
		case api.ActionStackStop:
			errs = append(errs, a.docker.Stop(ctx, c.ID))
		case api.ActionStackDown:
			errs = append(errs, a.docker.Remove(ctx, c.ID))
		}
	}
	if action == api.ActionStackDown {
		os.RemoveAll(a.stackDir(name))
	}
	return "", errors.Join(errs...)
}
