package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Maffewem/Orchestrator/internal/api"
)

// hardwareCaps are the capabilities this platform adds. Containers have no host to control.
func hardwareCaps() []string {
	if InContainer() {
		return nil
	}
	return []string{api.CapLED, api.CapConsole, api.CapSSH}
}

// hw holds the hardware controllers, created on first use.
type hardware struct {
	leds  *ledCtl
	ssh   *sshCtl
	power powerCache
}

func (a *Agent) hardware() *hardware {
	a.devhwOnce.Do(func() {
		dir := filepath.Dir(a.cfg.StatePath)
		if a.cfg.StatePath == "" {
			dir = os.TempDir()
		}
		a.devhw = &hardware{leds: newLEDCtl(dir, "/sys/class/leds", hostSampler()), ssh: newSSHCtl(dir)}
	})
	return a.devhw
}

// powerSource is the blade's supply, read only while the LED profile says this device is a blade.
func (a *Agent) powerSource() *api.PowerSource {
	if InContainer() {
		return nil
	}
	hw := a.hardware()
	hw.leds.mu.Lock()
	blade := hw.leds.cfg.Profile == api.LEDProfileBlade
	hw.leds.mu.Unlock()
	if !blade {
		return nil
	}
	return hw.power.get()
}

// startHardware runs the LED rules and the SSH trial watcher.
func (a *Agent) startHardware(ctx context.Context) {
	if InContainer() {
		return
	}
	hw := a.hardware()
	go hw.leds.run(ctx)
	go hw.ssh.watch(ctx, a.log.Info)
}

func jsonOut(v any) (string, error) {
	b, err := json.Marshal(v)
	return string(b), err
}

// hardwareCommand runs LED and SSH commands. handled is false for anything else.
func (a *Agent) hardwareCommand(ctx context.Context, c api.Command) (handled bool, out string, err error) {
	switch c.Action {
	case api.ActionLEDList, api.ActionLEDIdentify, api.ActionLEDConfig, api.ActionSSHStatus, api.ActionSSHCheck, api.ActionSSHConfig, api.ActionSSHConfirm, api.ActionSSHKey:
	default:
		return false, "", nil
	}
	if InContainer() {
		return true, "", errors.New("this agent runs in a container, so it cannot control the device's LEDs or SSH; install it as a service on the device")
	}
	hw := a.hardware()
	switch c.Action {
	case api.ActionLEDList:
		out, err = jsonOut(hw.leds.list())
	case api.ActionLEDIdentify:
		secs, _ := strconv.Atoi(c.Args["seconds"])
		var only []string
		if l := c.Args["led"]; l != "" {
			only = []string{l}
		}
		var stop func()
		if secs, stop, err = hw.leds.identifyStart(secs, only); err == nil {
			// The LEDs flash in the background so the command queue is not held up.
			time.AfterFunc(time.Duration(secs)*time.Second, stop)
			out = strconv.Itoa(secs)
		}
	case api.ActionLEDConfig:
		var cfg api.LEDConfig
		if err = json.Unmarshal([]byte(c.Args["config"]), &cfg); err == nil {
			err = hw.leds.setConfig(cfg)
		}
	case api.ActionSSHStatus:
		out, err = jsonOut(hw.ssh.status(ctx))
	case api.ActionSSHCheck:
		var s api.SSHSettings
		if err = json.Unmarshal([]byte(c.Args["settings"]), &s); err == nil {
			var ck api.SSHCheck
			if ck, err = hw.ssh.check(ctx, s); err == nil {
				out, err = jsonOut(ck)
			}
		}
	case api.ActionSSHConfig:
		var s api.SSHSettings
		if err = json.Unmarshal([]byte(c.Args["settings"]), &s); err == nil {
			trial, _ := strconv.Atoi(c.Args["trial"])
			out, err = hw.ssh.apply(ctx, s, c.Args["confirm"] == "1", trial)
		}
	case api.ActionSSHConfirm:
		out, err = hw.ssh.confirm(ctx, c.Args["revert"] == "1")
	case api.ActionSSHKey:
		switch c.Args["op"] {
		case "add":
			out, err = hw.ssh.addKey(c.Args["user"], c.Args["key"])
		case "remove":
			out, err = hw.ssh.removeKey(ctx, c.Args["user"], c.Args["fingerprint"], c.Args["confirm"] == "1")
		default:
			err = errors.New("ssh.key needs op add or remove")
		}
	}
	return true, strings.TrimSpace(out), err
}
