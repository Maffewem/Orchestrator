package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Maffewem/Orchestrator/internal/api"
)

type fakeFan struct {
	pct      int
	released bool
	fail     bool
	sets     []int
}

func (f *fakeFan) Info() api.FanInfo {
	return api.FanInfo{Driver: "fake", Controllable: true, Percent: f.pct}
}
func (f *fakeFan) Set(p int) error {
	if f.fail && p != 100 {
		return errors.New("write failed")
	}
	f.pct, f.released = p, false
	f.sets = append(f.sets, p)
	return nil
}
func (f *fakeFan) Release() error { f.released = true; return nil }

type fakeWork struct{ running map[string]bool }

func (w *fakeWork) Running(context.Context) ([]string, error) {
	var out []string
	for n, r := range w.running {
		if r {
			out = append(out, n)
		}
	}
	return out, nil
}
func (w *fakeWork) Stop(_ context.Context, n string) error  { w.running[n] = false; return nil }
func (w *fakeWork) Start(_ context.Context, n string) error { w.running[n] = true; return nil }

func rig(t *testing.T, fan fanDriver, temp *float64, ok *bool) (*thermalCtl, *time.Time, *fakeWork) {
	now := time.Unix(1000, 0)
	w := &fakeWork{running: map[string]bool{"batch": true, "db": true, "web": true}}
	c := newThermalCtl(t.TempDir(), func() (float64, bool) { return *temp, *ok }, fan, w)
	c.now = func() time.Time { return now }
	return c, &now, w
}

func TestCurveInterpolation(t *testing.T) {
	cv := []api.CurvePoint{{C: 40, Pct: 30}, {C: 60, Pct: 60}, {C: 80, Pct: 100}}
	for c, want := range map[float64]int{20: 30, 40: 30, 50: 45, 60: 60, 70: 80, 90: 100} {
		if got := interpolate(cv, c); got != want {
			t.Errorf("%v°C: got %d want %d", c, got, want)
		}
	}
}

func TestFanCurveMinimumAndCritical(t *testing.T) {
	temp, ok := 30.0, true
	fan := &fakeFan{}
	c, _, _ := rig(t, fan, &temp, &ok)
	c.SetPolicy(api.ThermalPolicy{Rev: 1, FanMode: "curve", MinPct: 30, CritC: 85, Curve: []api.CurvePoint{{C: 40, Pct: 10}, {C: 80, Pct: 100}}})
	c.Tick(context.Background())
	if fan.pct != 30 {
		t.Fatalf("minimum not applied: %d", fan.pct)
	}
	temp = 86
	c.Tick(context.Background())
	if fan.pct != 100 {
		t.Fatalf("critical must force full: %d", fan.pct)
	}
}

func TestAutoLeavesFanAloneAndReleases(t *testing.T) {
	temp, ok := 50.0, true
	fan := &fakeFan{}
	c, _, _ := rig(t, fan, &temp, &ok)
	c.SetPolicy(api.ThermalPolicy{Rev: 1, FanMode: "manual", FanManual: 50, MinPct: 30})
	c.Tick(context.Background())
	if fan.pct != 50 {
		t.Fatal("manual not applied")
	}
	c.SetPolicy(api.ThermalPolicy{Rev: 2, FanMode: "auto", MinPct: 30})
	c.Tick(context.Background())
	if !fan.released {
		t.Fatal("auto mode must hand the fan back")
	}
}

func TestSensorLossAndWriteFailureFailSafe(t *testing.T) {
	temp, ok := 50.0, false
	fan := &fakeFan{}
	c, _, _ := rig(t, fan, &temp, &ok)
	c.SetPolicy(api.ThermalPolicy{Rev: 1, FanMode: "curve", MinPct: 30, CritC: 85, Curve: []api.CurvePoint{{C: 40, Pct: 30}, {C: 80, Pct: 100}}})
	c.Tick(context.Background())
	if fan.pct != 100 || c.Status().Failsafe == "" {
		t.Fatalf("sensor loss must give full speed: %d %q", fan.pct, c.Status().Failsafe)
	}
	ok = true
	fan.fail = true
	c.Tick(context.Background())
	if fan.pct != 100 || !strings.Contains(c.Status().Failsafe, "fan write failed") {
		t.Fatalf("write failure must give full speed: %d %q", fan.pct, c.Status().Failsafe)
	}
}

func TestProtectionStopsSustainedHeatSparesCriticalAndResumes(t *testing.T) {
	temp, ok := 60.0, true
	fan := &fakeFan{}
	c, now, w := rig(t, fan, &temp, &ok)
	c.SetPolicy(api.ThermalPolicy{Rev: 1, FanMode: "auto", MinPct: 30, Critical: []string{"db"},
		Protect: api.ThermalProtect{Enabled: true, TempC: 75, ForSeconds: 60, ResumeBelowC: 65, FanFull: true, Stop: []string{"batch", "db"}}})
	ctx := context.Background()
	temp = 80
	c.Tick(ctx)
	*now = now.Add(30 * time.Second)
	c.Tick(ctx)
	if !w.running["batch"] || c.Status().Protecting {
		t.Fatal("must not act before the sustained period")
	}
	*now = now.Add(31 * time.Second)
	c.Tick(ctx)
	if w.running["batch"] || !w.running["db"] || !w.running["web"] || !c.Status().Protecting || fan.pct != 100 {
		t.Fatalf("protection wrong: %v fan %d", w.running, fan.pct)
	}
	// A dip below the trigger but above the resume point does not resume.
	temp = 70
	*now = now.Add(2 * time.Minute)
	c.Tick(ctx)
	if w.running["batch"] {
		t.Fatal("hysteresis violated")
	}
	temp = 60
	c.Tick(ctx)
	*now = now.Add(61 * time.Second)
	c.Tick(ctx)
	if !w.running["batch"] || c.Status().Protecting {
		t.Fatal("must resume once cooled")
	}
	var msgs []string
	for _, e := range c.Status().Events {
		msgs = append(msgs, e.Message)
	}
	all := strings.Join(msgs, "|")
	for _, want := range []string{"Stopped worker batch", "db is marked critical", "Started worker batch"} {
		if !strings.Contains(all, want) {
			t.Errorf("missing event %q in %s", want, all)
		}
	}
	n := len(c.Status().Events)
	c.Ack(n)
	if len(c.Status().Events) != 0 {
		t.Fatal("ack must drop sent events")
	}
}

func TestStoppedWorkersSurviveAgentRestart(t *testing.T) {
	temp, ok := 90.0, true
	fan := &fakeFan{}
	c, now, w := rig(t, fan, &temp, &ok)
	pol := api.ThermalPolicy{Rev: 1, FanMode: "auto", MinPct: 30, Protect: api.ThermalProtect{Enabled: true, TempC: 75, ForSeconds: 30, ResumeBelowC: 65, Stop: []string{"batch"}}}
	c.SetPolicy(pol)
	c.Tick(context.Background())
	*now = now.Add(31 * time.Second)
	c.Tick(context.Background())
	c2 := newThermalCtl(c.dir, func() (float64, bool) { return 50, true }, fan, w)
	if len(c2.stopped) != 1 || !c2.protecting || c2.policy.Rev != 1 {
		t.Fatalf("state not restored: %+v", c2.stopped)
	}
}

func TestHwmonAndPWMDrivers(t *testing.T) {
	root := t.TempDir()
	hw := filepath.Join(root, "sys/class/hwmon/hwmon3")
	os.MkdirAll(hw, 0o755)
	for f, v := range map[string]string{"name": "pwmfan", "pwm1": "128", "pwm1_enable": "2", "fan1_input": "2400"} {
		os.WriteFile(filepath.Join(hw, f), []byte(v), 0o644)
	}
	f := detectFan(root)
	if i := f.Info(); i.Driver != "hwmon" || i.RPM != 2400 || i.Percent != 50 {
		t.Fatalf("info %+v", i)
	}
	if err := f.Set(100); err != nil || readTrim(filepath.Join(hw, "pwm1")) != "255" || readTrim(filepath.Join(hw, "pwm1_enable")) != "1" {
		t.Fatal("set failed")
	}
	f.Release()
	if readTrim(filepath.Join(hw, "pwm1_enable")) != "2" {
		t.Fatal("original enable mode not restored")
	}

	chip := filepath.Join(root, "sys/class/pwm/pwmchip0/pwm0")
	os.MkdirAll(chip, 0o755)
	p := pwmFanFromEnv(root, "pwmchip0:0")
	if err := p.Set(50); err != nil || readTrim(filepath.Join(chip, "duty_cycle")) != "20000" || readTrim(filepath.Join(chip, "enable")) != "1" {
		t.Fatal("pwm set failed")
	}
}
