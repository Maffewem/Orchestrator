package agent

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"

	"github.com/Maffewem/Orchestrator/internal/api"
)

func fakeLEDs(t *testing.T) string {
	root := t.TempDir()
	for name, trig := range map[string]string{"ACT": "none mmc0 [mmc0] timer", "PWR": "none [default-on] timer", "mystery": "none [none]"} {
		d := filepath.Join(root, name)
		os.MkdirAll(d, 0o755)
		os.WriteFile(filepath.Join(d, "brightness"), []byte("1\n"), 0o644)
		os.WriteFile(filepath.Join(d, "max_brightness"), []byte("1\n"), 0o644)
		os.WriteFile(filepath.Join(d, "trigger"), []byte(trig+"\n"), 0o644)
	}
	return root
}

func TestLEDRolesAndTriggers(t *testing.T) {
	leds, note, err := newSysfsLEDs(fakeLEDs(t)).List()
	if err != nil || note != "" || len(leds) != 3 {
		t.Fatalf("list: %v %q %v", leds, note, err)
	}
	roles := map[string]string{}
	for _, l := range leds {
		roles[l.Name] = l.Role
	}
	if roles["ACT"] != "activity" || roles["PWR"] != "power" || roles["mystery"] != "other" {
		t.Fatalf("roles: %v", roles)
	}
	if leds[0].Trigger != "mmc0" {
		t.Fatalf("active trigger: %q", leds[0].Trigger)
	}
}

func TestLEDRestoreAfterOn(t *testing.T) {
	root := fakeLEDs(t)
	d := newSysfsLEDs(root)
	if err := d.Set("ACT", "on"); err != nil {
		t.Fatal(err)
	}
	if got := readTrim(filepath.Join(root, "ACT", "trigger")); got != "none" {
		t.Fatalf("trigger while on: %q", got)
	}
	d.Set("ACT", "restore")
	if got := readTrim(filepath.Join(root, "ACT", "trigger")); got != "mmc0" {
		t.Fatalf("trigger after restore: %q", got)
	}
	if d.Set("../x", "on") == nil || d.Set("nope", "on") == nil {
		t.Fatal("bad names must be refused")
	}
}

func TestLEDRulesWorstStateWins(t *testing.T) {
	root := fakeLEDs(t)
	vals := map[string]float64{"temp": 72, "cpu": 10}
	c := newLEDCtl(t.TempDir(), root, func() map[string]float64 { return vals })
	err := c.setConfig(api.LEDConfig{Profile: "pi", Enabled: true, Rules: []api.LEDRule{
		{Metric: "temp", LED: "ACT", Warn: 60, Crit: 80},
		{Metric: "cpu", LED: "ACT", Warn: 70, Crit: 90},
		{Metric: "disk", LED: "PWR", Warn: 80, Crit: 90}, // no sensor value: left alone
	}})
	if err != nil {
		t.Fatal(err)
	}
	st := c.list()
	for _, l := range st.LEDs {
		switch l.Name {
		case "ACT":
			if l.State != "warn" {
				t.Fatalf("ACT state %q", l.State)
			}
		case "PWR":
			if l.State != "" {
				t.Fatalf("PWR touched: %q", l.State)
			}
		}
	}
	vals["temp"] = 50 // back to normal: LED goes back to its own trigger
	c.mu.Lock()
	c.applyLocked()
	c.mu.Unlock()
	if got := readTrim(filepath.Join(root, "ACT", "trigger")); got != "mmc0" {
		t.Fatalf("not restored: %q", got)
	}
	c.setConfig(api.LEDConfig{Profile: "pi"})
	c.drv.Restore()
}

func TestLEDConfigValidation(t *testing.T) {
	for _, cfg := range []api.LEDConfig{
		{Profile: "x"},
		{Profile: "pi", Rules: []api.LEDRule{{Metric: "fan", LED: "ACT", Warn: 1, Crit: 2}}},
		{Profile: "pi", Rules: []api.LEDRule{{Metric: "temp", LED: "../ACT", Warn: 1, Crit: 2}}},
		{Profile: "pi", Rules: []api.LEDRule{{Metric: "cpu", LED: "ACT", Warn: 90, Crit: 80}}},
		{Profile: "pi", Rules: []api.LEDRule{{Metric: "cpu", LED: "ACT", Warn: 90, Crit: 120}}},
	} {
		if validLEDConfig(&cfg) == nil {
			t.Errorf("accepted %+v", cfg)
		}
	}
}

func TestIdentifyAndBladeHook(t *testing.T) {
	root := fakeLEDs(t)
	c := newLEDCtl(t.TempDir(), root, nil)
	secs, stop, err := c.identifyStart(1, nil)
	if err != nil || secs != 3 {
		t.Fatalf("identify: %d %v", secs, err)
	}
	if st := c.list(); st.LEDs[0].State != "identify" {
		t.Fatalf("state: %+v", st.LEDs[0])
	}
	stop()
	if got := readTrim(filepath.Join(root, "ACT", "trigger")); got != "mmc0" {
		t.Fatalf("not restored after identify: %q", got)
	}
	c.setConfig(api.LEDConfig{Profile: "blade", Enabled: true})
	if st := c.list(); st.Supported || !strings.Contains(st.Note, "/dev/pio0") {
		t.Fatalf("blade profile should say it is not supported: %+v", st)
	}
	if _, _, err := c.identifyStart(5, nil); err == nil {
		t.Fatal("identify on an unsupported profile must fail")
	}
}

// --- SSH ---

func testKey(t *testing.T, comment string) string {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	blob := []byte{}
	for _, part := range [][]byte{[]byte("ssh-ed25519"), pub} {
		blob = binary.BigEndian.AppendUint32(blob, uint32(len(part)))
		blob = append(blob, part...)
	}
	return "ssh-ed25519 " + base64.StdEncoding.EncodeToString(blob) + " " + comment
}

func TestParseKey(t *testing.T) {
	k := testKey(t, "me@laptop")
	typ, fp, cm, ok := parseKey(k)
	if !ok || typ != "ssh-ed25519" || !strings.HasPrefix(fp, "SHA256:") || cm != "me@laptop" {
		t.Fatalf("parse: %v %v %v %v", typ, fp, cm, ok)
	}
	for _, bad := range []string{
		"", "ssh-ed25519", `command="rm -rf /" ` + k, "ssh-rsa " + strings.SplitN(k, " ", 3)[1], // blob type differs
		k + "\nssh-ed25519 AAAA", "ssh-ed25519 !!!!",
	} {
		if _, _, _, ok := parseKey(bad); ok {
			t.Errorf("accepted %q", bad)
		}
	}
}

// fakeSSH is a pretend sshd: it reports the settings in the drop-in file, or defaults.
type fakeSSH struct {
	c      *sshCtl
	reload int
	failT  bool
}

func newFakeSSH(t *testing.T) (*fakeSSH, string) {
	home := t.TempDir()
	dir := t.TempDir()
	passwd := filepath.Join(dir, "passwd")
	os.WriteFile(passwd, []byte(fmt.Sprintf("root:x:0:0:root:%s/root:/bin/bash\npi:x:1000:1000::%s/pi:/bin/bash\ndaemon:x:1:1::/usr/sbin:/usr/sbin/nologin\n", home, home)), 0o644)
	os.MkdirAll(filepath.Join(home, "root"), 0o755)
	os.MkdirAll(filepath.Join(home, "pi"), 0o755)
	f := &fakeSSH{}
	c := &sshCtl{passwd: passwd, dropIn: filepath.Join(dir, "00-orchestrator.conf"), pending: filepath.Join(dir, "pending.json"), now: time.Now}
	c.run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		switch {
		case name == "systemctl" && args[0] == "is-active":
			return []byte("active\n"), nil
		case name == "systemctl" && args[0] == "reload":
			f.reload++
			return nil, nil
		case len(args) == 1 && args[0] == "-t":
			if f.failT {
				return []byte("bad config"), fmt.Errorf("exit 255")
			}
			return nil, nil
		case len(args) == 1 && args[0] == "-T":
			pw, pk, root := "yes", "yes", "prohibit-password"
			if b, err := os.ReadFile(c.dropIn); err == nil {
				for _, l := range strings.Split(string(b), "\n") {
					k, v, _ := strings.Cut(l, " ")
					switch k {
					case "PasswordAuthentication":
						pw = v
					case "PubkeyAuthentication":
						pk = v
					case "PermitRootLogin":
						root = v
					}
				}
			}
			return []byte(fmt.Sprintf("port 22\npasswordauthentication %s\npubkeyauthentication %s\npermitrootlogin %s\n", pw, pk, root)), nil
		}
		return nil, fmt.Errorf("unexpected %s %v", name, args)
	}
	f.c = c
	return f, home
}

func TestSSHLockoutRefusedWithoutKeys(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	os.WriteFile(filepath.Join(os.Getenv("PATH"), "sshd"), []byte("#!/bin/sh\n"), 0o755)
	f, _ := newFakeSSH(t)
	want := api.SSHSettings{PasswordAuth: false, PubkeyAuth: true, RootLogin: "no"}
	ck, err := f.c.check(context.Background(), want)
	if err != nil || !ck.Lockout {
		t.Fatalf("expected a lockout warning, got %+v %v", ck, err)
	}
	if _, err := f.c.apply(context.Background(), want, false, 60); err == nil || !strings.HasPrefix(err.Error(), "refused") {
		t.Fatalf("apply without confirmation: %v", err)
	}
	if _, err := os.Stat(f.c.dropIn); err == nil {
		t.Fatal("a refused change must not write anything")
	}
}

func TestSSHKeyOnlyTrialAndRevert(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	os.WriteFile(filepath.Join(os.Getenv("PATH"), "sshd"), []byte("#!/bin/sh\n"), 0o755)
	f, home := newFakeSSH(t)
	ctx := context.Background()
	key := testKey(t, "laptop")
	fp, err := f.c.addKey("pi", key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.c.addKey("pi", key); err == nil {
		t.Fatal("duplicate key accepted")
	}
	if _, err := f.c.addKey("daemon", key); err == nil {
		t.Fatal("key added for a non-login account")
	}
	if fi, _ := os.Stat(filepath.Join(home, "pi", ".ssh", "authorized_keys")); fi == nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("authorized_keys mode: %v", fi)
	}

	want := api.SSHSettings{PasswordAuth: false, PubkeyAuth: true, RootLogin: "prohibit-password"}
	out, err := f.c.apply(ctx, want, false, 60)
	if err != nil || !strings.Contains(out, "reverts in 60") {
		t.Fatalf("apply: %q %v", out, err)
	}
	if st := f.c.status(ctx); st.Pending == nil || st.Effective.PasswordAuth {
		t.Fatalf("status after apply: %+v", st)
	}
	// The last working key can't be removed while passwords are off, unless confirmed.
	if _, err := f.c.removeKey(ctx, "pi", fp, false); err == nil || !strings.HasPrefix(err.Error(), "refused") {
		t.Fatalf("removing the last key: %v", err)
	}
	// A second change can't start while one is on trial.
	if _, err := f.c.apply(ctx, api.SSHSettings{PasswordAuth: true, PubkeyAuth: true, RootLogin: "no"}, false, 60); err == nil {
		t.Fatal("overlapping change accepted")
	}
	// Not confirmed in time: it comes back by itself.
	f.c.now = func() time.Time { return time.Now().Add(2 * time.Minute) }
	cctx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	f.c.watch(cctx, func(string, ...any) {})
	if st := f.c.status(ctx); st.Pending != nil || !st.Effective.PasswordAuth {
		t.Fatalf("not reverted: %+v", st)
	}
	if _, err := os.Stat(f.c.dropIn); err == nil {
		t.Fatal("drop-in still there after revert")
	}
}

func TestSSHConfirmKeepsChange(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	os.WriteFile(filepath.Join(os.Getenv("PATH"), "sshd"), []byte("#!/bin/sh\n"), 0o755)
	f, _ := newFakeSSH(t)
	ctx := context.Background()
	f.c.addKey("pi", testKey(t, "a"))
	if _, err := f.c.apply(ctx, api.SSHSettings{PasswordAuth: false, PubkeyAuth: true, RootLogin: "no"}, false, 60); err != nil {
		t.Fatal(err)
	}
	if out, err := f.c.confirm(ctx, false); err != nil || out != "confirmed" {
		t.Fatalf("confirm: %q %v", out, err)
	}
	if st := f.c.status(ctx); st.Pending != nil || st.Effective.PasswordAuth {
		t.Fatalf("after confirm: %+v", st)
	}
	// Revert with nothing on trial removes the managed file: the way back to system settings.
	if _, err := f.c.confirm(ctx, true); err != nil {
		t.Fatal(err)
	}
	if st := f.c.status(ctx); !st.Effective.PasswordAuth || st.Managed {
		t.Fatalf("after revert: %+v", st)
	}
}

func TestSSHBadConfigIsUndone(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	os.WriteFile(filepath.Join(os.Getenv("PATH"), "sshd"), []byte("#!/bin/sh\n"), 0o755)
	f, _ := newFakeSSH(t)
	f.c.addKey("pi", testKey(t, "a"))
	f.failT = true
	if _, err := f.c.apply(context.Background(), api.SSHSettings{PasswordAuth: false, PubkeyAuth: true, RootLogin: "no"}, true, 0); err == nil {
		t.Fatal("expected sshd -t failure")
	}
	if _, err := os.Stat(f.c.dropIn); err == nil || f.reload != 0 {
		t.Fatal("a rejected config must be removed and never reloaded")
	}
}

func TestSSHKeysDontFollowSymlinks(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	f, home := newFakeSSH(t)
	target := t.TempDir()
	os.Symlink(target, filepath.Join(home, "pi", ".ssh"))
	if _, err := f.c.addKey("pi", testKey(t, "a")); err == nil {
		t.Fatal("wrote through a symlinked .ssh directory")
	}
	if entries, _ := os.ReadDir(target); len(entries) != 0 {
		t.Fatalf("symlink target modified: %v", entries)
	}
}

func TestConsolePTY(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "tty >/dev/null && echo orch-$((20+22)); stty size")
	m, err := startPTY(cmd, 100, 30)
	if err != nil {
		t.Skip("no pty here: ", err)
	}
	defer m.Close()
	var buf strings.Builder
	b := make([]byte, 256)
	for {
		n, err := m.Read(b)
		buf.Write(b[:n])
		if err != nil {
			break
		}
	}
	cmd.Wait()
	if !strings.Contains(buf.String(), "orch-42") || !strings.Contains(buf.String(), "30 100") {
		t.Fatalf("terminal output: %q", buf.String())
	}
}

// --- blade LEDs ---

type fakePixels struct {
	mu     sync.Mutex
	writes [][][3]byte
	closed bool
}

func (f *fakePixels) Write(px [][3]byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writes = append(f.writes, append([][3]byte(nil), px...))
	return nil
}
func (f *fakePixels) Close() error { f.mu.Lock(); f.closed = true; f.mu.Unlock(); return nil }
func (f *fakePixels) last() [][3]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.writes) == 0 {
		return nil
	}
	return f.writes[len(f.writes)-1]
}

func TestBladeLEDs(t *testing.T) {
	px := &fakePixels{}
	d := newBladeLEDs(func() bool { return true }, func() (pixelWriter, error) { return px, nil })
	leds, note, _ := d.List()
	if note != "" || len(leds) != 2 || leds[0].Name != "blade-top" || leds[1].Name != "blade-edge" {
		t.Fatalf("list: %v %q", leds, note)
	}
	if err := d.Set("blade-edge", "on"); err != nil {
		t.Fatal(err)
	}
	if got := px.last(); got[0] != ([3]byte{}) || got[1] != ([3]byte{0, 160, 0}) {
		t.Fatalf("edge on: %v", got)
	}
	d.Set("blade-top", "slow")
	if got := px.last(); got[0] != ([3]byte{255, 140, 0}) {
		t.Fatalf("top amber: %v", got)
	}
	if d.Set("ACT", "on") == nil || d.Set("blade-top", "sparkle") == nil {
		t.Fatal("bad names or patterns must be refused")
	}
	d.Restore()
	if !px.closed || px.last()[0] != ([3]byte{}) {
		t.Fatalf("restore should switch off and release: %v closed=%v", px.last(), px.closed)
	}
}

func TestBladeNeedsPIO(t *testing.T) {
	d := newBladeLEDs(func() bool { return false }, nil)
	if _, note, _ := d.List(); !strings.Contains(note, "/dev/pio0") {
		t.Fatalf("note: %q", note)
	}
}

func TestBladeStealthAndRules(t *testing.T) {
	px := &fakePixels{}
	prev := ledProfiles[api.LEDProfileBlade]
	ledProfiles[api.LEDProfileBlade] = func(string) ledDriver {
		return newBladeLEDs(func() bool { return true }, func() (pixelWriter, error) { return px, nil })
	}
	defer func() { ledProfiles[api.LEDProfileBlade] = prev }()
	vals := map[string]float64{"temp": 90}
	c := newLEDCtl(t.TempDir(), t.TempDir(), func() map[string]float64 { return vals })
	rule := api.LEDRule{Metric: "temp", LED: "blade-top", Warn: 60, Crit: 80}
	if err := c.setConfig(api.LEDConfig{Profile: "blade", Enabled: true, Rules: []api.LEDRule{rule}}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond) // critical: red, blinking
	seenRed := false
	px.mu.Lock()
	for _, w := range px.writes {
		if w[0] == ([3]byte{255, 0, 0}) {
			seenRed = true
		}
	}
	px.mu.Unlock()
	if !seenRed {
		t.Fatal("a critical rule should show red on the blade LED")
	}
	if err := c.setConfig(api.LEDConfig{Profile: "blade", Enabled: true, Stealth: true, Rules: []api.LEDRule{rule}}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if got := px.last(); got[0] != ([3]byte{}) || got[1] != ([3]byte{}) {
		t.Fatalf("stealth must keep both LEDs off: %v", got)
	}
}

// --- PIO ---

func TestPIORequestNumbers(t *testing.T) {
	// _IOW(102, nr, size): the direction bit, the size of the argument struct, the magic and the number.
	if reqAddProgram != 1<<30|68<<16|102<<8|11 || reqSMClaim != 1<<30|2<<16|102<<8|20 ||
		reqSMInit != 1<<30|20<<16|102<<8|30 || reqSMPut != 1<<30|8<<16|102<<8|41 || reqGPIOFunction != 1<<30|4<<16|102<<8|51 {
		t.Fatalf("request numbers: %#x %#x %#x %#x %#x", reqAddProgram, reqSMClaim, reqSMInit, reqSMPut, reqGPIOFunction)
	}
}

func TestWS2812Setup(t *testing.T) {
	cfg := ws2812Config(18, 0, 200_000_000)
	if cfg.clkdiv != 25<<16 {
		t.Fatalf("clock divider for 800 kHz at 200 MHz should be 25, got %#x", cfg.clkdiv)
	}
	if cfg.execctrl != 3<<12 || cfg.pinctrl != 1<<29|18<<10 || cfg.shiftctrl != 1<<17|24<<25|1<<30 {
		t.Fatalf("config: %+v", cfg)
	}
	// FIFO word: red, green, blue in the top 24 bits.
	if w := ws2812Word(0x11, 0x22, 0x33); w != 0x11223300 {
		t.Fatalf("word: %#x", w)
	}
}

// --- blade power source ---

func TestGPIOUapiLayout(t *testing.T) {
	// linux/gpio.h: gpio_v2_line_request is 592 bytes, gpio_v2_line_values 16, gpiochip_info 68.
	if unsafe.Sizeof(gpioLineRequest{}) != 592 || unsafe.Sizeof(gpioLineValues{}) != 16 || unsafe.Sizeof(gpioChipInfo{}) != 68 {
		t.Fatalf("sizes: %d %d %d", unsafe.Sizeof(gpioLineRequest{}), unsafe.Sizeof(gpioLineValues{}), unsafe.Sizeof(gpioChipInfo{}))
	}
	if reqGPIOGetLine != 0xc250b407 || reqGPIOGetValues != 0xc010b40e || reqGPIOChipInfo != 0x8044b401 {
		t.Fatalf("requests: %#x %#x %#x", reqGPIOGetLine, reqGPIOGetValues, reqGPIOChipInfo)
	}
}

func TestPowerSource(t *testing.T) {
	if p := powerFromLevel(true); p.Source != "5v" || p.MaxWatts != 15 {
		t.Fatalf("high: %+v", p)
	}
	if p := powerFromLevel(false); p.Source != "poe-plus" || p.MaxWatts != 30 {
		t.Fatalf("low: %+v", p)
	}
	calls := 0
	c := &powerCache{read: func() (bool, error) { calls++; return false, nil }}
	c.get()
	c.get()
	if calls != 1 {
		t.Fatalf("reads should be cached for a minute, got %d", calls)
	}
	c2 := &powerCache{read: func() (bool, error) { return false, fmt.Errorf("no chip") }}
	if c2.get() != nil {
		t.Fatal("an unreadable pin must report nothing, not a guess")
	}
}

func TestIdentifyRunsUntilStopped(t *testing.T) {
	root := fakeLEDs(t)
	c := newLEDCtl(t.TempDir(), root, nil)
	if _, err := c.identifyRun(maxIdentifySeconds*10, nil); err != nil {
		t.Fatal(err)
	}
	if c.idTimer == nil {
		t.Fatal("no safety timeout was set")
	}
	// A second start replaces the first rather than stacking.
	if _, err := c.identifyRun(600, nil); err != nil {
		t.Fatal(err)
	}
	if st := c.list(); st.LEDs[0].State != "identify" {
		t.Fatalf("not flashing: %+v", st.LEDs[0])
	}
	c.identifyOff()
	if st := c.list(); st.LEDs[0].State == "identify" || c.idStop != nil || c.idTimer != nil {
		t.Fatalf("still flashing after stopping: %+v", st.LEDs[0])
	}
	c.identifyOff() // stopping twice is harmless
}

func TestPulseLevel(t *testing.T) {
	if pulseLevel(0) != 0 {
		t.Fatal("a breath starts dark")
	}
	if got := pulseLevel(pulsePeriod / 2); got < 0.99 {
		t.Fatalf("peak %v", got)
	}
	prev := -1.0
	for d := time.Duration(0); d <= pulsePeriod/2; d += pulseStep {
		if l := pulseLevel(d); l < prev {
			t.Fatalf("fade up is not smooth at %v", d)
		} else {
			prev = l
		}
	}
}

func TestIdentifyInfo(t *testing.T) {
	root := fakeLEDs(t)
	a := &Agent{}
	a.devhwOnce.Do(func() {})
	a.devhw = &hardware{leds: newLEDCtl(t.TempDir(), root, nil)}
	if in := a.identifyInfo(); !in.Available || in.Method == "" {
		t.Fatalf("a Pi with ACT/PWR should be able to identify: %+v", in)
	}
	// No kernel LEDs at all: unavailable, with a reason.
	a.devhw = &hardware{leds: newLEDCtl(t.TempDir(), t.TempDir(), nil)}
	if in := a.identifyInfo(); in.Available || in.Reason == "" {
		t.Fatalf("a device with no LEDs must say why: %+v", in)
	}
	// A blade profile without /dev/pio0 (a CM4 blade) is unsupported and says so.
	a.devhw.leds.setConfig(api.LEDConfig{Profile: "blade", Enabled: true})
	if in := a.identifyInfo(); in.Available || !strings.Contains(in.Reason, "/dev/pio0") {
		t.Fatalf("blade without PIO: %+v", in)
	}
}

func TestBladeColoursAndBrightness(t *testing.T) {
	w := &fakePixels{}
	dr := newBladeLEDs(func() bool { return true }, func() (pixelWriter, error) { return w, nil }).(*bladeLEDs)
	dr.SetColours(api.LEDColours{Warn: "#00ff00", Normal: "#102030", Brightness: 50})
	if err := dr.Set("blade-top", "restore"); err != nil {
		t.Fatal(err)
	}
	if got := w.last(); got[0] != [3]byte{8, 16, 24} {
		t.Fatalf("normal colour at half brightness: %v", got)
	}
	dr.Set("blade-edge", "slow") // warning: the user's green, not the default amber
	dr.Restore()
	if c, _ := dr.bladeColour("slow"); c != [3]byte{0, 255, 0} {
		t.Fatalf("warn colour: %v", c)
	}
	if c, _ := dr.bladeColour("fast"); c != [3]byte{255, 0, 0} {
		t.Fatalf("an unset colour keeps its default: %v", c)
	}
	cfg := api.LEDConfig{Profile: "blade", Colours: &api.LEDColours{Warn: "red"}}
	if validLEDConfig(&cfg) == nil {
		t.Fatal("accepted a bad colour")
	}
	cfg = api.LEDConfig{Profile: "blade", Colours: &api.LEDColours{Brightness: 150}}
	if validLEDConfig(&cfg) == nil {
		t.Fatal("accepted a brightness over 100")
	}
}
