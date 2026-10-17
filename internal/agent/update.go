package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/Maffewem/Orchestrator/internal/api"
)

// maxTrialStarts is how many times a freshly updated agent may start without managing a
// single heartbeat before the previous binary is put back.
const maxTrialStarts = 3

// archName is the installer's architecture name for this build.
func archName() string {
	if runtime.GOARCH != "arm" {
		return runtime.GOARCH
	}
	if strings.HasPrefix(machine(), "armv6") {
		return "armv6"
	}
	return "armv7"
}

func (a *Agent) trialPath() string {
	return filepath.Join(filepath.Dir(a.cfg.StatePath), "update-trial")
}

// executablePath is a variable so tests can point it at a scratch file.
var executablePath = func() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(exe)
}

// selfUpdate downloads the control plane's agent build and swaps it in. It returns a
// message, and restart=true when the process should exit so systemd starts the new binary.
func (a *Agent) selfUpdate(ctx context.Context) (msg string, restart bool, err error) {
	if InContainer() {
		return "", false, errors.New("this agent runs in a container: pull a newer orchestrator-agent image instead")
	}
	exe, err := executablePath()
	if err != nil {
		return "", false, err
	}

	var info api.AgentUpdateInfo
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, a.cfg.Server+"/api/agent/update?arch="+archName(), nil)
	req.Header.Set("Authorization", "Bearer "+a.state.AgentToken)
	resp, err := a.http.Do(req)
	if err != nil {
		return "", false, err
	}
	err = json.NewDecoder(resp.Body).Decode(&info)
	resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusOK || info.SHA256 == "" {
		return "", false, fmt.Errorf("the control plane has no build for %s (HTTP %d)", archName(), resp.StatusCode)
	}
	if info.Version == Version {
		return "already running " + Version, false, nil
	}

	tmp := exe + ".new"
	defer os.Remove(tmp)
	if err := a.download(ctx, info.URL, tmp, info.SHA256); err != nil {
		return "", false, err
	}
	// A wrong-architecture or truncated binary would fail here, before it replaces anything.
	out, err := exec.CommandContext(ctx, tmp, "-version").Output()
	if err != nil || strings.TrimSpace(string(out)) != info.Version {
		return "", false, fmt.Errorf("the downloaded agent does not run on this device (%v)", err)
	}

	if err := copyFile(exe, exe+".prev", 0o755); err != nil {
		return "", false, fmt.Errorf("keep previous version: %w", err)
	}
	if err := os.WriteFile(a.trialPath(), []byte("0"), 0o600); err != nil {
		return "", false, err
	}
	if err := os.Rename(tmp, exe); err != nil {
		os.Remove(a.trialPath())
		return "", false, err
	}
	return fmt.Sprintf("updated %s to %s, restarting", Version, info.Version), true, nil
}

func (a *Agent) download(ctx context.Context, path, dest, wantSum string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.cfg.Server+path, nil)
	if err != nil {
		return err
	}
	resp, err := (&http.Client{}).Do(req) // no overall timeout: slow links
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download failed (HTTP %d)", resp.StatusCode)
	}
	f, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(f, h), resp.Body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if hex.EncodeToString(h.Sum(nil)) != wantSum {
		return errors.New("downloaded agent failed its checksum")
	}
	return nil
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

// checkTrial runs at startup. After an update the agent is on trial: if it keeps dying
// before its first heartbeat, the previous binary is restored and the process exits so
// systemd starts that one instead.
func (a *Agent) checkTrial() {
	b, err := os.ReadFile(a.trialPath())
	if err != nil {
		return
	}
	n, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	if n < maxTrialStarts {
		os.WriteFile(a.trialPath(), []byte(strconv.Itoa(n+1)), 0o600)
		return
	}
	exe, err := executablePath()
	if err == nil {
		if err = os.Rename(exe+".prev", exe); err == nil {
			a.log.Error("updated agent never came up; restored the previous version")
			os.Remove(a.trialPath())
			os.Exit(0)
		}
	}
	a.log.Error("updated agent never came up and the previous version could not be restored", "err", err)
	os.Remove(a.trialPath())
}

// confirmUpdate ends the trial period after the first successful heartbeat.
func (a *Agent) confirmUpdate() {
	if _, err := os.Stat(a.trialPath()); err == nil {
		os.Remove(a.trialPath())
		a.log.Info("update confirmed", "version", Version)
	}
}
