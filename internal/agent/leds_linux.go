package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Maffewem/Orchestrator/internal/api"
)

// Device LEDs. The agent drives the kernel's LED class (/sys/class/leds), which on a Raspberry
// Pi holds the green activity LED and the red power LED. It can flash them so a person can find
// the device ("identify"), and show CPU, memory, disk and temperature thresholds on them.
//
// Rules are kept on the device and evaluated by the agent, so they keep working while the
// server is unreachable. Every LED the agent touches is put back as it was found.

// ledDriver drives the LEDs of one kind of board. Pi-style boards use the kernel LED class;
// carriers whose LEDs sit behind another control method (GPIO, I2C or a vendor daemon) add a driver here.
type ledDriver interface {
	List() ([]api.LED, string, error) // LEDs, and a note when none can be driven
	Set(name, pattern string) error   // pattern: restore, off, on, slow or fast
	Restore()                         // put everything back
}

// ledProfiles is the hook for boards that need their own driver. A profile that is listed but has
// no driver says so in the dashboard instead of silently doing nothing.
var ledProfiles = map[string]func(root string) ledDriver{
	api.LEDProfilePi: func(root string) ledDriver { return newSysfsLEDs(root) },
	api.LEDProfileBlade: func(string) ledDriver {
		return newBladeLEDs(func() bool { _, err := os.Stat(bladePIODev); return err == nil }, rp1Opener)
	},
}

type unsupportedLEDs struct{ note string }

func (u unsupportedLEDs) List() ([]api.LED, string, error) { return nil, u.note, nil }
func (u unsupportedLEDs) Set(string, string) error         { return errors.New(u.note) }
func (u unsupportedLEDs) Restore()                         {}

// sysfsLEDs is the driver for /sys/class/leds.
type sysfsLEDs struct {
	root string
	mu   sync.Mutex
	orig map[string]ledOrig
	stop map[string]context.CancelFunc // running blinkers
}

type ledOrig struct {
	trigger    string
	brightness int
}

func newSysfsLEDs(root string) *sysfsLEDs {
	return &sysfsLEDs{root: root, orig: map[string]ledOrig{}, stop: map[string]context.CancelFunc{}}
}

var safeLEDName = func(s string) bool {
	if s == "" || len(s) > 64 || strings.ContainsAny(s, "/\\\x00") || s == "." || s == ".." {
		return false
	}
	return true
}

func ledRole(name string) string {
	n := strings.ToLower(name)
	switch {
	case n == "act" || n == "led0" || strings.Contains(n, "activity") || strings.Contains(n, "act"):
		return "activity"
	case n == "pwr" || n == "led1" || strings.Contains(n, "power") || strings.Contains(n, "pwr"):
		return "power"
	}
	return "other"
}

func readInt(p string) int {
	n, _ := strconv.Atoi(readTrim(p))
	return n
}

// parseTriggers splits a trigger file such as "none timer [mmc0] heartbeat" into the active one and all of them.
func parseTriggers(s string) (active string, all []string) {
	for _, f := range strings.Fields(s) {
		if strings.HasPrefix(f, "[") && strings.HasSuffix(f, "]") {
			f = strings.Trim(f, "[]")
			active = f
		}
		all = append(all, f)
	}
	return active, all
}

func (s *sysfsLEDs) List() ([]api.LED, string, error) {
	entries, err := os.ReadDir(s.root)
	if err != nil || len(entries) == 0 {
		return nil, "This device exposes no LEDs to the kernel (nothing in /sys/class/leds). Boards with LEDs behind another controller need their own LED profile.", nil
	}
	var out []api.LED
	for _, e := range entries {
		dir := filepath.Join(s.root, e.Name())
		l := api.LED{Name: e.Name(), Role: ledRole(e.Name()), Brightness: readInt(filepath.Join(dir, "brightness")), Max: readInt(filepath.Join(dir, "max_brightness"))}
		l.Trigger, l.Triggers = parseTriggers(readTrim(filepath.Join(dir, "trigger")))
		out = append(out, l)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, "", nil
}

func (s *sysfsLEDs) write(name, file, val string) error {
	return os.WriteFile(filepath.Join(s.root, name, file), []byte(val), 0o644)
}

// remember records how an LED was before the agent first touched it.
func (s *sysfsLEDs) remember(name string) {
	if _, ok := s.orig[name]; ok {
		return
	}
	dir := filepath.Join(s.root, name)
	trig, _ := parseTriggers(readTrim(filepath.Join(dir, "trigger")))
	s.orig[name] = ledOrig{trigger: trig, brightness: readInt(filepath.Join(dir, "brightness"))}
}

func (s *sysfsLEDs) Set(name, pattern string) error {
	if !safeLEDName(name) {
		return fmt.Errorf("invalid LED name %q", name)
	}
	if _, err := os.Stat(filepath.Join(s.root, name)); err != nil {
		return fmt.Errorf("no LED called %q on this device", name)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if cancel := s.stop[name]; cancel != nil {
		cancel()
		delete(s.stop, name)
	}
	s.remember(name)
	max := readInt(filepath.Join(s.root, name, "max_brightness"))
	if max <= 0 {
		max = 1
	}
	switch pattern {
	case "restore":
		o := s.orig[name]
		delete(s.orig, name)
		if o.trigger != "" && o.trigger != "none" {
			return s.write(name, "trigger", o.trigger)
		}
		if err := s.write(name, "trigger", "none"); err != nil {
			return err
		}
		return s.write(name, "brightness", strconv.Itoa(o.brightness))
	case "off", "on":
		if err := s.write(name, "trigger", "none"); err != nil {
			return err
		}
		v := 0
		if pattern == "on" {
			v = max
		}
		return s.write(name, "brightness", strconv.Itoa(v))
	case "slow", "fast", "identify":
		if err := s.write(name, "trigger", "none"); err != nil {
			return err
		}
		half := 600 * time.Millisecond
		if pattern == "fast" || pattern == "identify" {
			half = 120 * time.Millisecond
		}
		ctx, cancel := context.WithCancel(context.Background())
		s.stop[name] = cancel
		go func() {
			t := time.NewTicker(half)
			defer t.Stop()
			on := true
			for {
				v := 0
				if on {
					v = max
				}
				s.write(name, "brightness", strconv.Itoa(v))
				on = !on
				select {
				case <-ctx.Done():
					return
				case <-t.C:
				}
			}
		}()
		return nil
	}
	return fmt.Errorf("unknown LED pattern %q", pattern)
}

func (s *sysfsLEDs) Restore() {
	s.mu.Lock()
	names := make([]string, 0, len(s.orig))
	for n := range s.orig {
		names = append(names, n)
	}
	s.mu.Unlock()
	for _, n := range names {
		s.Set(n, "restore")
	}
}

// ledCtl owns the LED configuration and applies it.
type ledCtl struct {
	mu       sync.Mutex
	path     string // where the configuration is kept
	root     string
	cfg      api.LEDConfig
	drv      ledDriver
	state    map[string]string // what the rules want per LED: ok, warn, crit; identify while flashing
	values   map[string]float64
	identify map[string]bool // LEDs held by an identify run
	sample   func() map[string]float64
}

func newLEDCtl(dir, root string, sample func() map[string]float64) *ledCtl {
	c := &ledCtl{path: filepath.Join(dir, "leds.json"), root: root, sample: sample, state: map[string]string{}, identify: map[string]bool{}}
	if b, err := os.ReadFile(c.path); err == nil {
		json.Unmarshal(b, &c.cfg)
	}
	c.useProfile()
	return c
}

func (c *ledCtl) useProfile() {
	if c.cfg.Profile == "" {
		c.cfg.Profile = api.LEDProfilePi
	}
	mk := ledProfiles[c.cfg.Profile]
	if mk == nil {
		mk = ledProfiles[api.LEDProfilePi]
	}
	if c.drv != nil {
		c.drv.Restore()
	}
	c.drv = mk(c.root)
	c.state = map[string]string{}
}

func validLEDConfig(cfg *api.LEDConfig) error {
	if cfg.Profile == "" {
		cfg.Profile = api.LEDProfilePi
	}
	if ledProfiles[cfg.Profile] == nil {
		return fmt.Errorf("unknown LED profile %q", cfg.Profile)
	}
	if len(cfg.Rules) > 32 {
		return errors.New("too many LED rules (32 at most)")
	}
	for _, r := range cfg.Rules {
		switch r.Metric {
		case api.LEDMetricTemp, api.LEDMetricMemory, api.LEDMetricCPU, api.LEDMetricDisk:
		default:
			return fmt.Errorf("unknown metric %q", r.Metric)
		}
		if !safeLEDName(r.LED) {
			return fmt.Errorf("invalid LED name %q", r.LED)
		}
		if r.Warn <= 0 || r.Crit <= 0 || r.Crit < r.Warn {
			return fmt.Errorf("%s on %s: thresholds must be positive with the critical level at or above the warning level", r.Metric, r.LED)
		}
		if r.Metric != api.LEDMetricTemp && (r.Warn > 100 || r.Crit > 100) {
			return fmt.Errorf("%s on %s: a percentage can't be over 100", r.Metric, r.LED)
		}
	}
	return nil
}

// list answers led.list.
func (c *ledCtl) list() api.LEDStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	leds, note, err := c.drv.List()
	st := api.LEDStatus{Profile: c.cfg.Profile, Supported: note == "" && err == nil, Note: note, Config: c.cfg, LEDs: leds, Values: c.values}
	if err != nil {
		st.Note = err.Error()
	}
	if st.LEDs == nil {
		st.LEDs = []api.LED{}
	}
	for i := range st.LEDs {
		if c.identify[st.LEDs[i].Name] {
			st.LEDs[i].State = "identify"
		} else {
			st.LEDs[i].State = c.state[st.LEDs[i].Name]
		}
	}
	return st
}

// setConfig stores and applies a new configuration; turning rules off puts the LEDs back.
func (c *ledCtl) setConfig(cfg api.LEDConfig) error {
	if err := validLEDConfig(&cfg); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	os.MkdirAll(filepath.Dir(c.path), 0o755)
	b, _ := json.MarshalIndent(cfg, "", "  ")
	if err := os.WriteFile(c.path, b, 0o600); err != nil {
		return err
	}
	profileChanged := cfg.Profile != c.cfg.Profile
	c.cfg = cfg
	if profileChanged {
		c.useProfile()
	}
	c.applyLocked()
	return nil
}

// ledLevel gives the state a value puts an LED in.
func ledLevel(v float64, r api.LEDRule) string {
	switch {
	case v >= r.Crit:
		return "crit"
	case v >= r.Warn:
		return "warn"
	}
	return "ok"
}

var ledPattern = map[string]string{"ok": "restore", "warn": "slow", "crit": "fast", "stealth": "off"}

// applyLocked evaluates the rules and drives each LED to its worst state. Callers hold mu.
func (c *ledCtl) applyLocked() {
	want := map[string]string{}
	if c.cfg.Stealth {
		if leds, note, err := c.drv.List(); err == nil && note == "" {
			for _, l := range leds {
				want[l.Name] = "stealth"
			}
		}
	} else if c.cfg.Enabled {
		if c.sample != nil {
			c.values = c.sample()
		}
		rank := map[string]int{"ok": 0, "warn": 1, "crit": 2}
		for _, r := range c.cfg.Rules {
			v, ok := c.values[r.Metric]
			if !ok {
				continue // the device has no such sensor
			}
			lvl := ledLevel(v, r)
			if cur, seen := want[r.LED]; !seen || rank[lvl] > rank[cur] {
				want[r.LED] = lvl
			}
		}
	}
	// LEDs that had a state and have none now go back to normal.
	for name, was := range c.state {
		if _, ok := want[name]; !ok && was != "" && !c.identify[name] {
			c.drv.Set(name, "restore")
			delete(c.state, name)
		}
	}
	for name, lvl := range want {
		if c.identify[name] || c.state[name] == lvl {
			continue
		}
		if err := c.drv.Set(name, ledPattern[lvl]); err != nil {
			continue
		}
		c.state[name] = lvl
	}
}

// run evaluates the rules every few seconds until ctx ends, then puts the LEDs back.
func (c *ledCtl) run(ctx context.Context) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	c.mu.Lock()
	c.applyLocked()
	c.mu.Unlock()
	for {
		select {
		case <-ctx.Done():
			c.mu.Lock()
			c.drv.Restore()
			c.mu.Unlock()
			return
		case <-t.C:
			c.mu.Lock()
			if c.cfg.Enabled {
				c.applyLocked()
			}
			c.mu.Unlock()
		}
	}
}

// identifyStart starts flashing the activity and power LEDs (or the named ones) fast and
// returns how long it will go on; stop puts them back and lets the rules take over again.
func (c *ledCtl) identifyStart(seconds int, only []string) (secs int, stop func(), err error) {
	if seconds < 3 {
		seconds = 3
	}
	if seconds > 60 {
		seconds = 60
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	leds, note, err := c.drv.List()
	if err != nil {
		return 0, nil, err
	}
	if note != "" {
		return 0, nil, errors.New(note)
	}
	var targets []string
	for _, l := range leds {
		if len(only) > 0 {
			for _, o := range only {
				if o == l.Name {
					targets = append(targets, l.Name)
				}
			}
		} else if l.Role != "other" {
			targets = append(targets, l.Name)
		}
	}
	if len(targets) == 0 {
		return 0, nil, errors.New("this device has no activity or power LED to flash")
	}
	var ok []string
	for _, n := range targets {
		if c.drv.Set(n, "identify") == nil {
			c.identify[n] = true
			ok = append(ok, n)
		}
	}
	if len(ok) == 0 {
		return 0, nil, errors.New("the LEDs could not be switched (does the agent run as root?)")
	}
	return seconds, func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		for _, n := range ok {
			delete(c.identify, n)
			c.drv.Set(n, "restore")
			delete(c.state, n)
		}
		c.applyLocked()
	}, nil
}

// hostSample reads the metrics LED rules can use. CPU is measured between two calls.
func hostSampler() func() map[string]float64 {
	var lastTotal, lastIdle uint64
	return func() map[string]float64 {
		out := map[string]float64{}
		total, idle := cpuTimes()
		if lastTotal > 0 && total > lastTotal {
			dt, di := total-lastTotal, idle-lastIdle
			out[api.LEDMetricCPU] = float64(dt-di) / float64(dt) * 100
		}
		lastTotal, lastIdle = total, idle
		mem := meminfo()
		if t := mem["MemTotal"]; t > 0 {
			if av, ok := mem["MemAvailable"]; ok && av <= t {
				out[api.LEDMetricMemory] = float64(t-av) / float64(t) * 100
			}
		}
		if t := temperatureC(); t > 0 {
			out[api.LEDMetricTemp] = t
		}
		if used, total := rootDisk(); total > 0 {
			out[api.LEDMetricDisk] = float64(used) / float64(total) * 100
		}
		return out
	}
}

func temperatureC() float64 {
	zones, _ := filepath.Glob("/sys/class/thermal/thermal_zone*/temp")
	var hottest float64
	for _, z := range zones {
		if m, err := strconv.ParseFloat(readTrim(z), 64); err == nil && m/1000 > hottest {
			hottest = m / 1000
		}
	}
	return hottest
}

func rootDisk() (used, total uint64) {
	var st syscall.Statfs_t
	if syscall.Statfs("/", &st) != nil {
		return 0, 0
	}
	bs := uint64(st.Bsize)
	return (st.Blocks - st.Bfree) * bs, st.Blocks * bs
}
