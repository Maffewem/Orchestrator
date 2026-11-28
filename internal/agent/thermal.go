package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Maffewem/Orchestrator/internal/api"
)

// The thermal controller runs inside the agent, on its own timer, and applies the policy the
// server last sent (kept on disk). It therefore keeps working when the server is unreachable.
// Whenever it cannot trust what it sees (no temperature, a fan write fails) it drives the fan
// to full speed rather than leave a hot device with a slow fan.

const (
	thermalTick   = 5 * time.Second
	resumeHold    = time.Minute
	maxThermalEvt = 50
)

// fanDriver is one way of reading and setting a fan.
type fanDriver interface {
	Info() api.FanInfo
	// Set takes control of the fan and sets its duty (0-100).
	Set(pct int) error
	// Release gives the fan back to whatever controlled it before Set.
	Release() error
}

// workloads is what the controller needs from Docker.
type workloads interface {
	Running(ctx context.Context) ([]string, error)
	Stop(ctx context.Context, name string) error
	Start(ctx context.Context, name string) error
}

type thermalCtl struct {
	mu                  sync.Mutex
	now                 func() time.Time
	temp                func() (float64, bool)
	fan                 fanDriver // nil when the device has no fan the agent knows
	work                workloads
	dir                 string // where policy.json and stopped.json are kept
	policy              api.ThermalPolicy
	taken               bool // we have set the fan and must Release it
	hotSince, coolSince time.Time
	protecting          bool
	stopped             []string
	failsafe            string
	target              int
	events              []api.ThermalEvent
	lastErr             string
}

func newThermalCtl(dir string, temp func() (float64, bool), fan fanDriver, w workloads) *thermalCtl {
	t := &thermalCtl{now: time.Now, temp: temp, fan: fan, work: w, dir: dir, target: -1}
	t.load()
	return t
}

func (t *thermalCtl) load() {
	if t.dir == "" {
		return
	}
	if b, err := os.ReadFile(filepath.Join(t.dir, "thermal-policy.json")); err == nil {
		_ = json.Unmarshal(b, &t.policy)
	}
	if b, err := os.ReadFile(filepath.Join(t.dir, "thermal-stopped.json")); err == nil {
		_ = json.Unmarshal(b, &t.stopped)
		t.protecting = len(t.stopped) > 0
	}
}

func (t *thermalCtl) save(name string, v any) {
	if t.dir == "" {
		return
	}
	b, _ := json.Marshal(v)
	tmp := filepath.Join(t.dir, name+".tmp")
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, filepath.Join(t.dir, name))
	}
}

func (t *thermalCtl) event(level, format string, args ...any) {
	t.events = append(t.events, api.ThermalEvent{At: t.now(), Level: level, Message: fmt.Sprintf(format, args...)})
	if len(t.events) > maxThermalEvt {
		t.events = t.events[len(t.events)-maxThermalEvt:]
	}
}

// SetPolicy installs a policy from the server; an unchanged revision is ignored.
func (t *thermalCtl) SetPolicy(p api.ThermalPolicy) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if p.Rev == t.policy.Rev && p.Rev != 0 {
		return
	}
	t.policy = p
	t.save("thermal-policy.json", p)
}

// Interpolate returns the duty for a temperature on a curve (points sorted by temperature).
// Below the first point it holds the first duty; above the last it holds the last.
func interpolate(curve []api.CurvePoint, c float64) int {
	if len(curve) == 0 {
		return 100
	}
	if c <= curve[0].C {
		return curve[0].Pct
	}
	for i := 1; i < len(curve); i++ {
		if c <= curve[i].C {
			a, b := curve[i-1], curve[i]
			if b.C == a.C {
				return b.Pct
			}
			f := (c - a.C) / (b.C - a.C)
			return a.Pct + int(f*float64(b.Pct-a.Pct)+0.5)
		}
	}
	return curve[len(curve)-1].Pct
}

// Tick runs one control step.
func (t *thermalCtl) Tick(ctx context.Context) {
	t.mu.Lock()
	defer t.mu.Unlock()
	p := t.policy
	c, ok := t.temp()
	now := t.now()

	// Fan.
	want := -1 // -1: leave the fan alone
	t.failsafe = ""
	if t.fan != nil && t.fan.Info().Controllable {
		switch {
		case !ok && (p.FanMode == "manual" || p.FanMode == "curve" || t.protecting):
			want, t.failsafe = 100, "no temperature reading"
		case p.FanMode == "manual":
			want = max(p.FanManual, p.MinPct)
		case p.FanMode == "curve":
			want = max(interpolate(p.Curve, c), p.MinPct)
		}
		if ok && p.CritC > 0 && c >= p.CritC && want >= 0 {
			want = 100
		}
		if t.protecting && p.Protect.FanFull {
			want = 100
		}
		if p.Protect.Enabled && ok && p.Protect.FanFull && !t.protecting && p.Protect.TempC > 0 && c >= p.Protect.TempC && !t.hotSince.IsZero() && now.Sub(t.hotSince) >= time.Duration(p.Protect.ForSeconds)*time.Second {
			want = 100
		}
	}
	t.applyFan(want)

	t.protect(ctx, p, c, ok, now)
}

func (t *thermalCtl) applyFan(want int) {
	if t.fan == nil {
		t.target = -1
		return
	}
	if want < 0 {
		if t.taken {
			if err := t.fan.Release(); err != nil {
				t.event("warning", "Could not hand the fan back to its controller: %v", err)
			} else {
				t.event("info", "Fan control released; the fan is back under its own controller")
			}
			t.taken = false
		}
		t.target = -1
		return
	}
	if want == t.target && t.taken {
		return
	}
	if err := t.fan.Set(want); err != nil {
		// Fail safe: try full speed, and say so.
		t.failsafe = "fan write failed: " + err.Error()
		if want != 100 {
			_ = t.fan.Set(100)
			want = 100
		}
		if t.lastErr != t.failsafe {
			t.event("critical", "Fan control failed (%v); the fan was set to full speed", err)
		}
		t.lastErr = t.failsafe
	} else {
		t.lastErr = ""
		if t.failsafe != "" && want == 100 {
			t.event("warning", "Fan set to full speed: %s", t.failsafe)
		}
	}
	t.taken = true
	t.target = want
}

func (t *thermalCtl) protect(ctx context.Context, p api.ThermalPolicy, c float64, ok bool, now time.Time) {
	pr := p.Protect
	if !pr.Enabled || pr.TempC <= 0 {
		t.hotSince = time.Time{}
		if t.protecting {
			t.resume(ctx, "protection was turned off")
		}
		return
	}
	hot := ok && c >= pr.TempC
	if hot {
		if t.hotSince.IsZero() {
			t.hotSince = now
		}
		t.coolSince = time.Time{}
	} else {
		t.hotSince = time.Time{}
	}
	if !t.protecting && hot && now.Sub(t.hotSince) >= time.Duration(pr.ForSeconds)*time.Second {
		t.protecting = true
		t.event("critical", "Thermal protection triggered: %.0f°C for %ds (limit %.0f°C)", c, pr.ForSeconds, pr.TempC)
		if pr.FanFull {
			t.event("warning", "Fan set to full speed by thermal protection")
		}
		t.stopWorkers(ctx, p)
		return
	}
	if t.protecting {
		if hot {
			// Still hot: pick up anything that started again.
			t.stopWorkers(ctx, p)
			t.coolSince = time.Time{}
			return
		}
		below := ok && c < pr.ResumeBelowC
		if !below {
			t.coolSince = time.Time{}
			return
		}
		if t.coolSince.IsZero() {
			t.coolSince = now
		}
		if now.Sub(t.coolSince) >= resumeHold {
			t.resume(ctx, fmt.Sprintf("temperature is %.0f°C, below %.0f°C", c, pr.ResumeBelowC))
		}
	}
}

func (t *thermalCtl) stopWorkers(ctx context.Context, p api.ThermalPolicy) {
	if t.work == nil || len(p.Protect.Stop) == 0 {
		return
	}
	running, err := t.work.Running(ctx)
	if err != nil {
		return
	}
	for _, name := range p.Protect.Stop {
		if slices.Contains(p.Critical, name) {
			t.event("info", "Worker %s is marked critical and was not stopped", name)
			continue
		}
		if !slices.Contains(running, name) {
			continue
		}
		if err := t.work.Stop(ctx, name); err != nil {
			t.event("warning", "Could not stop %s: %v", name, err)
			continue
		}
		if !slices.Contains(t.stopped, name) {
			t.stopped = append(t.stopped, name)
		}
		t.event("critical", "Stopped worker %s to cool the device", name)
	}
	t.save("thermal-stopped.json", t.stopped)
}

func (t *thermalCtl) resume(ctx context.Context, why string) {
	var left []string
	for _, name := range t.stopped {
		if t.work == nil {
			break
		}
		if err := t.work.Start(ctx, name); err != nil {
			t.event("warning", "Could not start %s again: %v", name, err)
			left = append(left, name)
			continue
		}
		t.event("info", "Started worker %s again (%s)", name, why)
	}
	if len(t.stopped) == 0 {
		t.event("info", "Thermal protection ended (%s)", why)
	}
	t.stopped = left
	t.protecting = len(left) > 0
	t.coolSince = time.Time{}
	t.save("thermal-stopped.json", t.stopped)
}

// Status is the snapshot for the heartbeat. Ack drops the events once the server has them.
func (t *thermalCtl) Status() *api.ThermalStatus {
	t.mu.Lock()
	defer t.mu.Unlock()
	st := &api.ThermalStatus{PolicyRev: t.policy.Rev, Mode: t.policy.FanMode, Target: t.target, Failsafe: t.failsafe,
		Protecting: t.protecting, Stopped: slices.Clone(t.stopped), Events: slices.Clone(t.events)}
	if t.fan != nil {
		fi := t.fan.Info()
		st.Fan = &fi
	}
	return st
}

func (t *thermalCtl) Ack(n int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if n > len(t.events) {
		n = len(t.events)
	}
	t.events = t.events[n:]
}

// ---- drivers ----

// hwmonFan drives a fan that Linux exposes as hwmon pwm1 (Raspberry Pi 5 official fan, many
// fan controller chips such as the EMC2101/EMC2301 when their kernel driver is loaded).
type hwmonFan struct {
	dir, name  string
	origEnable string
	origPWM    string
}

func (f *hwmonFan) Info() api.FanInfo {
	i := api.FanInfo{Driver: "hwmon", Name: f.name, Controllable: true, Percent: -1}
	if v, err := strconv.Atoi(readTrim(filepath.Join(f.dir, "pwm1"))); err == nil {
		i.Percent = (v*100 + 127) / 255
	}
	if v, err := strconv.Atoi(readTrim(filepath.Join(f.dir, "fan1_input"))); err == nil {
		i.RPM = v
	}
	return i
}

func (f *hwmonFan) Set(pct int) error {
	if f.origEnable == "" {
		f.origEnable = readTrim(filepath.Join(f.dir, "pwm1_enable"))
		f.origPWM = readTrim(filepath.Join(f.dir, "pwm1"))
	}
	if _, err := os.Stat(filepath.Join(f.dir, "pwm1_enable")); err == nil {
		if err := os.WriteFile(filepath.Join(f.dir, "pwm1_enable"), []byte("1"), 0o644); err != nil {
			return err
		}
	}
	return os.WriteFile(filepath.Join(f.dir, "pwm1"), []byte(strconv.Itoa((pct*255+50)/100)), 0o644)
}

func (f *hwmonFan) Release() error {
	var err error
	if f.origEnable != "" {
		err = os.WriteFile(filepath.Join(f.dir, "pwm1_enable"), []byte(f.origEnable), 0o644)
	}
	f.origEnable, f.origPWM = "", ""
	return err
}

// coolingFan only reports a kernel cooling device that looks like a fan; the kernel's thermal
// governor owns it, so the agent does not try to write it.
type coolingFan struct{ dir, name string }

func (f *coolingFan) Info() api.FanInfo {
	i := api.FanInfo{Driver: "cooling", Name: f.name, Percent: -1, Note: "Read-only: the kernel thermal governor controls this fan"}
	cur, e1 := strconv.Atoi(readTrim(filepath.Join(f.dir, "cur_state")))
	mx, e2 := strconv.Atoi(readTrim(filepath.Join(f.dir, "max_state")))
	if e1 == nil && e2 == nil && mx > 0 {
		i.Percent = cur * 100 / mx
	}
	return i
}
func (f *coolingFan) Set(int) error  { return fmt.Errorf("read-only fan") }
func (f *coolingFan) Release() error { return nil }

// bladeFan drives a Compute Blade fan unit through bladectl (`set fan --percent N`, `set fan --auto`), because the fan unit is not a
// Linux hwmon device. The command lines are configurable (ORCH_BLADE_FAN_SET with {percent},
// ORCH_BLADE_FAN_RELEASE) because they could not be checked against real hardware.
type bladeFan struct {
	bin, set, release string
	last              int
}

func (f *bladeFan) Info() api.FanInfo {
	return api.FanInfo{Driver: "bladectl", Name: "Compute Blade fan unit", Controllable: true, Percent: f.last,
		Note: "Commands match bladectl --help; not yet tried against a running fan unit"}
}

func (f *bladeFan) run(line string) error {
	args := strings.Fields(line)
	if len(args) == 0 {
		return fmt.Errorf("no command configured")
	}
	cmd := exec.Command(args[0], args[1:]...)
	// bladectl finds the blade agent through ~/.config/bladectl/config.yaml, which the blade
	// agent writes for root. Under systemd (or sudo that keeps the caller's HOME) it would
	// otherwise look in the wrong place and fail with "produced zero addresses".
	if os.Geteuid() == 0 {
		cmd.Env = append(os.Environ(), "HOME=/root")
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %v: %s", args[0], err, strings.TrimSpace(string(out)))
	}
	return nil
}
func (f *bladeFan) Set(pct int) error {
	if err := f.run(strings.ReplaceAll(f.set, "{percent}", strconv.Itoa(pct))); err != nil {
		return err
	}
	f.last = pct
	return nil
}
func (f *bladeFan) Release() error { f.last = -1; return f.run(f.release) }

// pwmFan drives a fan wired to a kernel PWM channel through /sys/class/pwm, such as the
// standard Compute Blade fan unit (GPIO12/13 in alternate PWM mode, which needs the
// pwm-2chan device-tree overlay). It is opt-in (ORCH_PWM_FAN=pwmchip0:0) because a PWM chip
// could be driving anything. A 4-wire PWM fan wants about 25 kHz. The tachometer is not read.
type pwmFan struct {
	dir  string // .../pwmchipN/pwmM
	last int
}

const pwmPeriodNS = 40000 // 25 kHz

func (f *pwmFan) Info() api.FanInfo {
	return api.FanInfo{Driver: "pwm", Name: "PWM fan (" + filepath.Base(filepath.Dir(f.dir)) + ")", Controllable: true, Percent: f.last,
		Note: "Duty is set directly; the RPM is not read. The agent sets it to 100% when it lets go."}
}

func (f *pwmFan) write(name, v string) error {
	return os.WriteFile(filepath.Join(f.dir, name), []byte(v), 0o644)
}

func (f *pwmFan) Set(pct int) error {
	if err := f.write("period", strconv.Itoa(pwmPeriodNS)); err != nil {
		return err
	}
	if err := f.write("duty_cycle", strconv.Itoa(pwmPeriodNS*pct/100)); err != nil {
		return err
	}
	if err := f.write("enable", "1"); err != nil {
		return err
	}
	f.last = pct
	return nil
}

// Release leaves the fan at full speed: nothing else is in charge of a bare PWM channel.
func (f *pwmFan) Release() error { return f.Set(100) }

// pwmFanFromEnv exports and opens the channel named by spec ("pwmchip0:0").
func pwmFanFromEnv(root, spec string) fanDriver {
	chip, ch, ok := strings.Cut(spec, ":")
	if !ok || chip == "" || ch == "" {
		return nil
	}
	base := filepath.Join(root, "sys/class/pwm", chip)
	dir := filepath.Join(base, "pwm"+ch)
	if _, err := os.Stat(dir); err != nil {
		if err := os.WriteFile(filepath.Join(base, "export"), []byte(ch), 0o644); err != nil {
			return nil
		}
		for i := 0; i < 20; i++ { // the kernel creates the directory a moment later
			if _, err := os.Stat(dir); err == nil {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	return &pwmFan{dir: dir, last: -1}
}

// detectFan finds a fan the agent can use; root is the host filesystem root.
func detectFan(root string) fanDriver {
	if spec := os.Getenv("ORCH_PWM_FAN"); spec != "" {
		if f := pwmFanFromEnv(root, spec); f != nil {
			return f
		}
	}
	pwms, _ := filepath.Glob(filepath.Join(root, "sys/class/hwmon/hwmon*/pwm1"))
	for _, p := range pwms {
		dir := filepath.Dir(p)
		name := readTrim(filepath.Join(dir, "name"))
		if name == "" {
			name = "fan"
		}
		return &hwmonFan{dir: dir, name: name + " fan"}
	}
	if bin, err := exec.LookPath("bladectl"); err == nil {
		set := os.Getenv("ORCH_BLADE_FAN_SET")
		if set == "" {
			set = bin + " set fan --percent {percent}"
		}
		rel := os.Getenv("ORCH_BLADE_FAN_RELEASE")
		if rel == "" {
			rel = bin + " set fan --auto"
		}
		return &bladeFan{bin: bin, set: set, release: rel, last: -1}
	}
	cool, _ := filepath.Glob(filepath.Join(root, "sys/class/thermal/cooling_device*"))
	for _, d := range cool {
		typ := readTrim(filepath.Join(d, "type"))
		if strings.Contains(strings.ToLower(typ), "fan") {
			return &coolingFan{dir: d, name: typ}
		}
	}
	return nil
}

// dockerWork adapts Docker to the controller. Names are container names.
type dockerWork struct{ d *Docker }

func (w dockerWork) Running(ctx context.Context) ([]string, error) {
	cs, err := w.d.Containers(ctx)
	var out []string
	for _, c := range cs {
		if c.State == "running" {
			out = append(out, strings.TrimPrefix(c.Name, "/"))
		}
	}
	return out, err
}
func (w dockerWork) id(ctx context.Context, name string) (string, error) {
	id, err := w.d.ContainerByName(ctx, name)
	if err == nil && id == "" {
		err = fmt.Errorf("no container named %s", name)
	}
	return id, err
}
func (w dockerWork) Stop(ctx context.Context, name string) error {
	id, err := w.id(ctx, name)
	if err != nil {
		return err
	}
	return w.d.Stop(ctx, id)
}
func (w dockerWork) Start(ctx context.Context, name string) error {
	id, err := w.id(ctx, name)
	if err != nil {
		return err
	}
	return w.d.Start(ctx, id)
}

// hottest is the hottest sensor the agent can read.
func (a *Agent) hottest() (float64, bool) {
	var m float64
	for _, s := range a.sensors() {
		m = max(m, s.C)
	}
	return m, m > 0
}

func (a *Agent) thermalLoop(ctx context.Context) {
	dir := filepath.Dir(a.cfg.StatePath)
	a.thermal = newThermalCtl(dir, a.hottest, detectFan(cmpRoot(a.cfg.HostRoot)), dockerWork{a.docker})
	tk := time.NewTicker(thermalTick)
	defer tk.Stop()
	for {
		a.thermal.Tick(ctx)
		select {
		case <-ctx.Done():
			// Leave the fan under its own controller when the agent stops cleanly.
			a.thermal.mu.Lock()
			if a.thermal.fan != nil && a.thermal.taken {
				_ = a.thermal.fan.Release()
			}
			a.thermal.mu.Unlock()
			return
		case <-tk.C:
		}
	}
}

func cmpRoot(r string) string {
	if r == "" {
		return "/"
	}
	return r
}
