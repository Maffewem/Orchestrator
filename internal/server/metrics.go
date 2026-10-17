package server

import (
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
	s := append(h.data[deviceID], MetricSample{
		Time:       time.Now().UTC(),
		CPUPercent: m.CPUPercent,
		MemPercent: pct(m.MemoryUsed, m.MemoryTotal),
		DiskPerc:   pct(m.DiskUsed, m.DiskTotal),
		Load1:      m.Load1,
	})
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
