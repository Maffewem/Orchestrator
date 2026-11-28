package agent

import (
	"context"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Maffewem/Orchestrator/internal/api"
)

// Disk, network, power and memory-pressure figures for the device graphs. Counters come from /proc
// and /sys; rates are the change since the previous report, so the first report has none.

// hwState remembers the previous counters.
type hwState struct {
	at     time.Time
	disks  map[string][7]uint64 // reads, sectors read, writes, sectors written, ms busy
	nets   map[string][3]uint64 // rx bytes, tx bytes, errors+drops
	cpuTot uint64
	cpuIO  uint64

	vcgen     time.Time // when vcgencmd was last asked
	vcgenVal  uint32
	vcgenOK   bool
	vcgenMiss bool // vcgencmd is not installed
}

// sysFile maps a /proc or /sys path onto the host root when the agent runs in a container that
// mounts the host there, and falls back to the agent's own view.
func (a *Agent) sysFile(p string) string {
	if a.cfg.HostRoot != "" {
		h := filepath.Join(a.cfg.HostRoot, p)
		if _, err := os.Stat(h); err == nil {
			return h
		}
	}
	return p
}

// hardwareMetrics fills the extended figures of m.
func (a *Agent) hardwareMetrics(m *api.Metrics) {
	now := time.Now()
	st := &a.hw
	elapsed := now.Sub(st.at).Seconds()
	haveRate := !st.at.IsZero() && elapsed >= 1

	// CPU time spent waiting for I/O.
	if tot, io := a.cpuIOWait(); tot > 0 {
		if st.cpuTot > 0 && tot > st.cpuTot && io >= st.cpuIO {
			m.IOWaitPercent = float64(io-st.cpuIO) / float64(tot-st.cpuTot) * 100
		}
		st.cpuTot, st.cpuIO = tot, io
	}

	// Swap and kernel out-of-memory kills.
	mem := meminfo()
	m.SwapTotal = mem["SwapTotal"]
	if f, ok := mem["SwapFree"]; ok && f <= m.SwapTotal {
		m.SwapUsed = m.SwapTotal - f
	}
	m.OOMKills = vmstatValue(readTrim(a.sysFile("/proc/vmstat")), "oom_kill")

	// CPU clock.
	cpu0 := a.sysFile("/sys/devices/system/cpu/cpu0/cpufreq")
	if khz, err := strconv.ParseFloat(readTrim(filepath.Join(cpu0, "scaling_cur_freq")), 64); err == nil {
		m.ClockMHz = khz / 1000
	}
	if khz, err := strconv.ParseFloat(readTrim(filepath.Join(cpu0, "cpuinfo_max_freq")), 64); err == nil {
		m.MaxClockMHz = khz / 1000
	}

	m.Power = a.powerState(now)

	disks := parseDiskstats(readTrim(a.sysFile("/proc/diskstats")), func(name string) bool {
		_, err := os.Stat(a.sysFile("/sys/block/" + name))
		return err == nil
	})
	nets := parseNetDev(readTrim(a.sysFile("/proc/net/dev")))
	wifi := parseWireless(readTrim(a.sysFile("/proc/net/wireless")))

	newDisks := map[string][7]uint64{}
	for name, c := range disks {
		newDisks[name] = c
		prev, ok := st.disks[name]
		if !haveRate || !ok {
			continue
		}
		d := func(i int) float64 { return float64(c[i]-prev[i]) / elapsed }
		m.Disks = append(m.Disks, api.DiskIO{
			Name: name, ReadBps: d(1) * 512, WriteBps: d(3) * 512, ReadIOPS: d(0), WriteIOPS: d(2),
			Busy: math.Min(100, float64(c[4]-prev[4])/(elapsed*1000)*100),
		})
	}
	sortBy(m.Disks, func(x api.DiskIO) string { return x.Name })

	newNets := map[string][3]uint64{}
	for name, c := range nets {
		newNets[name] = c
		prev, ok := st.nets[name]
		n := api.NetIO{Name: name}
		if lvl, w := wifi[name]; w {
			n.Wireless, n.SignalDBm = true, lvl
		}
		if haveRate && ok && c[0] >= prev[0] && c[1] >= prev[1] {
			n.RxBps = float64(c[0]-prev[0]) / elapsed
			n.TxBps = float64(c[1]-prev[1]) / elapsed
			if c[2] >= prev[2] {
				n.Errors = float64(c[2]-prev[2]) / elapsed
			}
		} else if !haveRate || !ok {
			// No rate yet, but a wireless signal is still worth showing.
			if !n.Wireless {
				continue
			}
		}
		m.Nets = append(m.Nets, n)
	}
	sortBy(m.Nets, func(x api.NetIO) string { return x.Name })

	st.at, st.disks, st.nets = now, newDisks, newNets
}

func sortBy[T any](s []T, key func(T) string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && key(s[j]) < key(s[j-1]); j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

func (a *Agent) cpuIOWait() (total, iowait uint64) {
	line := readTrim(a.sysFile("/proc/stat"))
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	f := strings.Fields(line)
	if len(f) < 6 || f[0] != "cpu" {
		return 0, 0
	}
	for i, v := range f[1:] {
		n, _ := strconv.ParseUint(v, 10, 64)
		total += n
		if i == 4 {
			iowait = n
		}
	}
	return
}

func vmstatValue(text, key string) uint64 {
	for _, l := range strings.Split(text, "\n") {
		k, v, ok := strings.Cut(l, " ")
		if ok && k == key {
			n, _ := strconv.ParseUint(strings.TrimSpace(v), 10, 64)
			return n
		}
	}
	return 0
}

// parseDiskstats returns counters for whole physical disks: reads, sectors read, writes, sectors
// written and milliseconds spent busy. isDisk says whether a name is a whole block device (not a partition).
func parseDiskstats(text string, isDisk func(string) bool) map[string][7]uint64 {
	out := map[string][7]uint64{}
	for _, l := range strings.Split(text, "\n") {
		f := strings.Fields(l)
		if len(f) < 13 {
			continue
		}
		name := f[2]
		if strings.HasPrefix(name, "loop") || strings.HasPrefix(name, "ram") || strings.HasPrefix(name, "zram") || strings.HasPrefix(name, "dm-") || strings.HasPrefix(name, "nbd") {
			continue
		}
		if isDisk != nil && !isDisk(name) {
			continue
		}
		n := func(i int) uint64 { v, _ := strconv.ParseUint(f[i], 10, 64); return v }
		out[name] = [7]uint64{n(3), n(5), n(7), n(9), n(12)}
	}
	return out
}

// parseNetDev returns receive bytes, transmit bytes and errors plus drops per interface. Loopback
// and the virtual interfaces that Docker creates are left out.
func parseNetDev(text string) map[string][3]uint64 {
	out := map[string][3]uint64{}
	for _, l := range strings.Split(text, "\n") {
		name, rest, ok := strings.Cut(l, ":")
		if !ok {
			continue
		}
		name = strings.TrimSpace(name)
		if name == "lo" || strings.HasPrefix(name, "veth") || strings.HasPrefix(name, "br-") || name == "docker0" {
			continue
		}
		f := strings.Fields(rest)
		if len(f) < 16 {
			continue
		}
		n := func(i int) uint64 { v, _ := strconv.ParseUint(f[i], 10, 64); return v }
		out[name] = [3]uint64{n(0), n(8), n(2) + n(3) + n(10) + n(11)}
	}
	return out
}

// parseWireless reads /proc/net/wireless and returns the signal level in dBm per interface.
func parseWireless(text string) map[string]int {
	out := map[string]int{}
	for _, l := range strings.Split(text, "\n") {
		name, rest, ok := strings.Cut(l, ":")
		if !ok {
			continue
		}
		f := strings.Fields(rest)
		if len(f) < 3 {
			continue
		}
		lvl, err := strconv.ParseFloat(strings.TrimSuffix(f[2], "."), 64)
		if err != nil {
			continue
		}
		if lvl > 0 { // some drivers report 0-255 rather than dBm
			lvl -= 256
		}
		out[strings.TrimSpace(name)] = int(lvl)
	}
	return out
}

// powerState reads the Raspberry Pi firmware's throttling word, from sysfs when the kernel
// exposes it and otherwise from vcgencmd (asked at most every 10 s). Other hardware returns nil.
func (a *Agent) powerState(now time.Time) *api.PowerInfo {
	for _, p := range []string{"/sys/devices/platform/soc/soc:firmware/get_throttled", "/sys/devices/platform/soc/soc:firmware/raspberrypi-hwmon/hwmon/hwmon0/get_throttled"} {
		if raw, ok := parseThrottled(readTrim(a.sysFile(p))); ok {
			return api.DecodePower(raw)
		}
	}
	st := &a.hw
	if st.vcgenMiss {
		return nil
	}
	if now.Sub(st.vcgen) < 10*time.Second {
		if st.vcgenOK {
			return api.DecodePower(st.vcgenVal)
		}
		return nil
	}
	st.vcgen = now
	path, err := exec.LookPath("vcgencmd")
	if err != nil {
		st.vcgenMiss = true
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "get_throttled").Output()
	if err != nil {
		st.vcgenOK = false
		return nil
	}
	raw, ok := parseThrottled(strings.TrimPrefix(strings.TrimSpace(string(out)), "throttled="))
	st.vcgenVal, st.vcgenOK = raw, ok
	if !ok {
		return nil
	}
	return api.DecodePower(raw)
}

// parseThrottled accepts "0x50005", "50005" (the kernel prints bare hex) and trailing newlines.
func parseThrottled(s string) (uint32, bool) {
	s = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s), "0x"))
	if s == "" {
		return 0, false
	}
	n, err := strconv.ParseUint(s, 16, 32)
	return uint32(n), err == nil
}
