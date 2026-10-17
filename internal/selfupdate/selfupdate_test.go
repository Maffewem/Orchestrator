package selfupdate

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/Maffewem/Orchestrator/internal/agent"
)

func TestSwapAndRollback(t *testing.T) {
	for _, img := range []string{"alpine:3.20", "nginx:alpine"} {
		if exec.Command("docker", "image", "inspect", img).Run() != nil {
			t.Skipf("%s not available locally", img)
		}
	}
	docker := func(args ...string) string {
		out, err := exec.Command("docker", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("docker %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	const name = "su-test"
	clean := func() {
		exec.Command("docker", "rm", "-f", name, name+"-previous").Run()
	}
	clean()
	t.Cleanup(clean)
	docker("run", "-d", "--name", name, "--restart", "unless-stopped", "-e", "KEEP=me", "-p", "18099:80",
		"alpine:3.20", "sleep", "300")

	host := os.Getenv("DOCKER_HOST")
	if host == "" {
		host = "unix:///var/run/docker.sock"
	}
	d, err := agent.NewDocker(host)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	opts := SwapOptions{Container: name, Image: "nginx:alpine", Timeout: 20 * time.Second, Stable: 2 * time.Second}

	if err := Swap(ctx, d, opts); err != nil {
		t.Fatal(err)
	}
	if got := docker("inspect", "-f", "{{.Config.Image}}|{{.State.Running}}|{{.HostConfig.RestartPolicy.Name}}", name); got != "nginx:alpine|true|unless-stopped" {
		t.Fatalf("after swap: %s", got)
	}
	if got := docker("inspect", "-f", "{{range .Config.Env}}{{.}} {{end}}", name); !strings.Contains(got, "KEEP=me") {
		t.Fatalf("env lost: %s", got)
	}
	if got := docker("port", name); !strings.Contains(got, "18099") {
		t.Fatalf("port lost: %s", got)
	}
	if got := docker("inspect", "-f", "{{.State.Running}}|{{.HostConfig.RestartPolicy.Name}}", name+"-previous"); got != "false|no" {
		t.Fatalf("previous: %s", got)
	}

	// A new image that can't be created rolls back to the version that was running.
	opts.Image = "orchestrator-test/does-not-exist:1"
	err = Swap(ctx, d, opts)
	if err == nil || !strings.Contains(err.Error(), "rolled back") {
		t.Fatalf("expected rollback error, got %v", err)
	}
	if got := docker("inspect", "-f", "{{.Config.Image}}|{{.State.Running}}", name); got != "nginx:alpine|true" {
		t.Fatalf("after rollback: %s", got)
	}
}
