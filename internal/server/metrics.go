package server

import (
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/Maffewem/Orchestrator/internal/api"
)

// MetricSample is one point on a device's resource graphs.
type MetricSample struct {
	Time       time.Time `json:"time"`
	CPUPercent float64   `json:"cpu"`
	MemPercent float64   `json:"mem"`
	DiskPerc   float64   `json:"disk"`
	Load1      float64   `json:"load1"`
	// Temp is the hottest sensor in °C; 0 when the device reports none.
	Temp float64 `json:"temp"`

	// Extended figures. Zero means the agent did not report them (an older agent, or no such hardware).
	IOWait   float64 `json:"iowait,omitempty"`    // % of CPU time waiting for I/O
	Swap     float64 `json:"swap,omitempty"`      // % of swap in use
	SwapUsed uint64  `json:"swap_used,omitempty"` // bytes
	OOM      uint64  `json:"oom,omitempty"`       // kernel OOM kills since boot
	Clock    float64 `json:"clock,omitempty"`     // CPU clock, MHz
	MaxClock float64 `json:"max_clock,omitempty"` // highest allowed CPU clock, MHz
	// Power is the Raspberry Pi get_throttled word; HasPower tells 0 (healthy) from "not reported".
	Power    uint32       `json:"power,omitempty"`
	HasPower bool         `json:"has_power,omitempty"`
	Disks    []api.DiskIO `json:"disks,omitempty"`
	Nets     []api.NetIO  `json:"nets,omitempty"`
}

// metricsHistory keeps recent samples per device in memory. It is lost on restart, which
// is fine for live graphs; long-term metrics belong in a proper TSDB later.
type metricsHistory struct {
	mu   sync.Mutex
	size int
	data map[string][]MetricSample
}

func newMetricsHistory(size int) *metricsHistory {
	return &metricsHistory{size: size, data: map[string][]MetricSample{}}
}

func pct(used, total uint64) float64 {
	if total == 0 {
		return 0
	}
	return float64(used) / float64(total) * 100
}

func (h *metricsHistory) add(deviceID string, m api.Metrics) {
	h.mu.Lock()
	defer h.mu.Unlock()
	smp := MetricSample{
		Time:       time.Now().UTC(),
		CPUPercent: m.CPUPercent,
		MemPercent: pct(m.MemoryUsed, m.MemoryTotal),
		DiskPerc:   pct(m.DiskUsed, m.DiskTotal),
		Load1:      m.Load1,
		Temp:       m.TempC,
		IOWait:     m.IOWaitPercent,
		Swap:       pct(m.SwapUsed, m.SwapTotal),
		SwapUsed:   m.SwapUsed,
		OOM:        m.OOMKills,
		Clock:      m.ClockMHz,
		MaxClock:   m.MaxClockMHz,
		Disks:      m.Disks,
		Nets:       m.Nets,
	}
	if m.Power != nil {
		smp.Power, smp.HasPower = m.Power.Raw, true
	}
	s := append(h.data[deviceID], smp)
	if len(s) > h.size {
		s = s[len(s)-h.size:]
	}
	h.data[deviceID] = s
}

func (h *metricsHistory) get(deviceID string) []MetricSample {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]MetricSample{}, h.data[deviceID]...)
}

func (s *Server) handleDeviceMetrics(w http.ResponseWriter, r *http.Request) {
	d, ok := s.lookupDevice(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, s.metrics.get(d.ID))
}

// hardwareIssues turns recent samples and the latest report into issues for Needs attention and
// the alerts bell. Power and memory-kill events are read from the last hour of samples, so a
// problem that happened between two reports is still shown, and clears itself an hour later.
func (s *Server) hardwareIssues(deviceID string, m api.Metrics) (warnings []string) {
	if s.metrics == nil {
		return nil
	}
	if p := m.Power; p != nil {
		recent := s.metrics.get(deviceID)
		var seenUV, seenCap, seenThr, seenTemp bool
		prev := uint32(0)
		for i, x := range recent {
			if !x.HasPower {
				continue
			}
			// A flag counts when it is set now, or when its "since boot" bit rose between two samples.
			rose := func(now, since uint32) bool {
				return x.Power&now != 0 || (i > 0 && x.Power&since != 0 && prev&since == 0)
			}
			seenUV = seenUV || rose(1<<0, 1<<16)
			seenCap = seenCap || rose(1<<1, 1<<17)
			seenThr = seenThr || rose(1<<2, 1<<18)
			seenTemp = seenTemp || rose(1<<3, 1<<19)
			prev = x.Power
		}
		switch {
		case p.UnderVoltage:
			warnings = append(warnings, "Under-voltage right now: the power supply or cable can't deliver enough, so the CPU is being slowed. Use the official supply or a shorter, thicker cable")
		case seenUV:
			warnings = append(warnings, "Under-voltage was detected in the last hour. Check the power supply and cable")
		}
		switch {
		case p.Throttled || p.TempLimit:
			warnings = append(warnings, "The CPU is being throttled right now (under-voltage or heat). Check power and cooling")
		case p.FreqCapped:
			warnings = append(warnings, "The CPU clock is capped by the firmware right now (power or heat)")
		case seenThr || seenTemp || seenCap:
			warnings = append(warnings, "The CPU was throttled or clock-capped in the last hour")
		}
	}
	// Kernel out-of-memory kills: any rise of the counter within the retained samples.
	var lo, hi uint64
	for i, x := range s.metrics.get(deviceID) {
		if i == 0 || x.OOM < lo {
			lo = x.OOM // a reboot resets the counter, so measure from the lowest value seen
		}
		hi = x.OOM
	}
	if hi > lo {
		warnings = append(warnings, fmt.Sprintf("The kernel killed %d process(es) for running out of memory in the last hour. Add swap, raise container memory limits, or move a worker", hi-lo))
	}
	if m.SwapTotal > 0 && pct(m.SwapUsed, m.SwapTotal) >= 80 {
		warnings = append(warnings, fmt.Sprintf("Swap is %.0f%% full: the device is short of memory", pct(m.SwapUsed, m.SwapTotal)))
	}
	if m.IOWaitPercent >= 40 {
		warnings = append(warnings, fmt.Sprintf("The CPU is spending %.0f%% of its time waiting for the disk. A failing or overloaded SD card or drive is likely", m.IOWaitPercent))
	}
	return warnings
}
