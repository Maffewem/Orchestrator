package agent

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/Maffewem/Orchestrator/internal/api"
)

// meminfo parses /proc/meminfo into bytes.
func meminfo() map[string]uint64 {
	out := map[string]uint64{}
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return out
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		fields := strings.Fields(v)
		if len(fields) == 0 {
			continue
		}
		n, _ := strconv.ParseUint(fields[0], 10, 64)
		out[k] = n * 1024
	}
	return out
}

// cpuTimes returns total and idle jiffies from /proc/stat.
func cpuTimes() (total, idle uint64) {
	line := readTrim("/proc/stat")
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	fields := strings.Fields(line)
	if len(fields) < 5 || fields[0] != "cpu" {
		return 0, 0
	}
	for i, f := range fields[1:] {
		n, _ := strconv.ParseUint(f, 10, 64)
		total += n
		if i == 3 || i == 4 { // idle, iowait
			idle += n
		}
	}
	return total, idle
}

func (a *Agent) metrics() api.Metrics {
	var m api.Metrics
	mem := meminfo()
	m.MemoryTotal = mem["MemTotal"]
	if avail, ok := mem["MemAvailable"]; ok && avail <= m.MemoryTotal {
		m.MemoryUsed = m.MemoryTotal - avail
	}

	total, idle := cpuTimes()
	if a.lastCPUTotal > 0 && total > a.lastCPUTotal {
		dt, di := total-a.lastCPUTotal, idle-a.lastCPUIdle
		m.CPUPercent = float64(dt-di) / float64(dt) * 100
	}
	a.lastCPUTotal, a.lastCPUIdle = total, idle

	if f := strings.Fields(readTrim("/proc/loadavg")); len(f) >= 3 {
		m.Load1, _ = strconv.ParseFloat(f[0], 64)
		m.Load5, _ = strconv.ParseFloat(f[1], 64)
		m.Load15, _ = strconv.ParseFloat(f[2], 64)
	}
	if f := strings.Fields(readTrim("/proc/uptime")); len(f) >= 1 {
		up, _ := strconv.ParseFloat(f[0], 64)
		m.UptimeSeconds = uint64(up)
	}

	m.Sensors = a.sensors()
	for _, s := range m.Sensors {
		if s.C > m.TempC {
			m.TempC = s.C
		}
	}

	var st syscall.Statfs_t
	if syscall.Statfs(a.hostPath("/"), &st) == nil {
		bs := uint64(st.Bsize)
		m.DiskTotal = st.Blocks * bs
		m.DiskUsed = (st.Blocks - st.Bfree) * bs
	}
	a.hardwareMetrics(&m)
	return m
}

// sensors returns every temperature the kernel exposes: thermal zones (the SoC on a Pi) and
// hwmon chips (NVMe drives, add-in boards). Readings of zero or less are ignored.
func (a *Agent) sensors() []api.TempSensor {
	var out []api.TempSensor
	seen := map[string]int{}
	add := func(name string, file string) {
		milli, err := strconv.ParseFloat(readTrim(file), 64)
		if err != nil || milli <= 0 || milli > 200000 {
			return
		}
		if name == "" {
			name = "sensor"
		}
		seen[name]++
		if n := seen[name]; n > 1 {
			name = fmt.Sprintf("%s %d", name, n)
		}
		out = append(out, api.TempSensor{Name: name, C: milli / 1000})
	}
	zones, _ := filepath.Glob(a.hostPath("/sys/class/thermal/thermal_zone*/temp"))
	for _, z := range zones {
		add(readTrim(filepath.Join(filepath.Dir(z), "type")), z)
	}
	inputs, _ := filepath.Glob(a.hostPath("/sys/class/hwmon/hwmon*/temp*_input"))
	for _, in := range inputs {
		dir := filepath.Dir(in)
		name := readTrim(filepath.Join(dir, "name"))
		// A thermal zone often mirrors the same chip; skip chips already reported that way.
		if name == "cpu_thermal" || name == "soc_thermal" {
			continue
		}
		if label := readTrim(strings.TrimSuffix(in, "_input") + "_label"); label != "" {
			name += " " + label
		}
		add(name, in)
	}
	return out
}
