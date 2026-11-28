package server

import (
	"strings"
	"testing"

	"github.com/Maffewem/Orchestrator/internal/api"
)

func TestHardwareIssues(t *testing.T) {
	s := &Server{metrics: newMetricsHistory(100)}
	has := func(w []string, sub string) bool {
		for _, x := range w {
			if strings.Contains(x, sub) {
				return true
			}
		}
		return false
	}
	m := api.Metrics{Power: api.DecodePower(0)}
	s.metrics.add("a", m)
	if w := s.hardwareIssues("a", m); len(w) != 0 {
		t.Fatalf("healthy device raised %v", w)
	}
	// Under-voltage seen between two reports: the "since boot" bit rose, the live bit is already clear.
	m = api.Metrics{Power: api.DecodePower(1 << 16)}
	s.metrics.add("a", m)
	if w := s.hardwareIssues("a", m); !has(w, "Under-voltage was detected") {
		t.Fatalf("missed a past under-voltage: %v", w)
	}
	// Live under-voltage with throttling.
	m = api.Metrics{Power: api.DecodePower(0x50005)}
	s.metrics.add("a", m)
	w := s.hardwareIssues("a", m)
	if !has(w, "Under-voltage right now") || !has(w, "throttled right now") {
		t.Fatalf("live flags: %v", w)
	}
	// A device that only carries old "since boot" bits from before the server saw it raises nothing.
	s.metrics.add("b", api.Metrics{Power: api.DecodePower(0x50000)})
	if w := s.hardwareIssues("b", api.Metrics{Power: api.DecodePower(0x50000)}); len(w) != 0 {
		t.Fatalf("stale boot flags raised %v", w)
	}
}

func TestHardwareIssuesMemoryPressure(t *testing.T) {
	s := &Server{metrics: newMetricsHistory(100)}
	s.metrics.add("a", api.Metrics{OOMKills: 2})
	m := api.Metrics{OOMKills: 4, SwapTotal: 100, SwapUsed: 90, IOWaitPercent: 55}
	s.metrics.add("a", m)
	w := strings.Join(s.hardwareIssues("a", m), "|")
	for _, want := range []string{"killed 2 process", "Swap is 90%", "55% of its time waiting"} {
		if !strings.Contains(w, want) {
			t.Errorf("missing %q in %q", want, w)
		}
	}
	// Counter falling after a reboot is not a kill.
	s.metrics.add("c", api.Metrics{OOMKills: 9})
	m = api.Metrics{OOMKills: 0}
	s.metrics.add("c", m)
	if w := s.hardwareIssues("c", m); len(w) != 0 {
		t.Fatalf("reboot raised %v", w)
	}
}

func TestSampleKeepsHardwareFigures(t *testing.T) {
	h := newMetricsHistory(10)
	h.add("a", api.Metrics{Disks: []api.DiskIO{{Name: "sda", ReadBps: 5}}, Power: api.DecodePower(0), ClockMHz: 1500})
	got := h.get("a")
	if len(got) != 1 || !got[0].HasPower || got[0].Clock != 1500 || got[0].Disks[0].Name != "sda" {
		t.Fatalf("sample = %+v", got)
	}
}
