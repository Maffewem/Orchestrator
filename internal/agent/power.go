package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"time"

	"github.com/Maffewem/Orchestrator/internal/api"
)

// powerAction validates a device.power command and, when it is acceptable, queues the real work to
// run after the result has been reported. The report has to go first: the machine may be the one the
// server runs on, and stopping Docker there would take the server (and the chance to report) with it.
func (a *Agent) powerAction(c api.Command) (string, error) {
	mode := c.Args["mode"]
	if mode != "poweroff" && mode != "reboot" {
		return "", fmt.Errorf("unknown power mode %q", mode)
	}
	if c.Args["confirm"] != mode {
		return "", errors.New("not confirmed: the confirmation must equal the mode")
	}
	if err := powerSupported(); err != nil {
		return "", err
	}
	a.afterReport = func() { a.runPower(mode) }
	if mode == "poweroff" {
		return "Shutting down: containers are stopped first.", nil
	}
	return "Restarting: containers are stopped first and start again after boot.", nil
}

// powerSupported says why this agent can't control the machine it runs on.
func powerSupported() error {
	if runtime.GOOS != "linux" {
		return errors.New("shutdown and restart are only supported on Linux devices")
	}
	if _, err := os.Stat("/run/systemd/system"); err != nil {
		return errors.New("this device does not use systemd, so the agent can't shut it down safely; do it on the device")
	}
	if _, err := exec.LookPath("systemctl"); err != nil {
		return errors.New("systemctl was not found on this device")
	}
	return nil
}

// runPower stops Docker (every container gets its stop signal and grace period, and keeps its restart
// policy, so "unless-stopped" workers come back after a restart), flushes the disks and asks systemd.
// If systemd refuses, Docker is started again so the device is not left half stopped.
func (a *Agent) runPower(mode string) {
	log := a.log.With("power", mode)
	log.Warn("stopping Docker before the machine goes down")
	sctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	if out, err := exec.CommandContext(sctx, "systemctl", "stop", "docker.service", "docker.socket").CombinedOutput(); err != nil {
		log.Warn("stopping Docker failed; systemd will stop it", "err", err, "out", string(out))
	}
	cancel()
	exec.Command("sync").Run()
	// A short pause lets the agent's own log lines and any open connections drain.
	time.Sleep(2 * time.Second)
	if out, err := exec.Command("systemctl", mode, "--no-block").CombinedOutput(); err != nil {
		log.Error("systemctl refused; starting Docker again", "err", err, "out", string(out))
		exec.Command("systemctl", "start", "docker.socket", "docker.service").Run()
	}
}
