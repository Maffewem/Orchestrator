package server

import (
	"context"
	"testing"
	"time"

	"github.com/Maffewem/Orchestrator/internal/api"
)

func TestAverageSamples(t *testing.T) {
	t0 := time.Unix(1000, 0).UTC()
	a := MetricSample{Time: t0, CPUPercent: 10, Temp: 50, OOM: 1, HasPower: true, Power: 1, Nets: []api.NetIO{{Name: "eth0", RxBps: 100}}}
	b := MetricSample{Time: t0.Add(5 * time.Second), CPUPercent: 30, Temp: 60, OOM: 3, HasPower: true, Power: 4, Nets: []api.NetIO{{Name: "eth0", RxBps: 300}}}
	o := averageSamples([]MetricSample{a, b})
	if o.CPUPercent != 20 || o.Temp != 60 || o.OOM != 3 || o.Power != 5 || len(o.Nets) != 1 || o.Nets[0].RxBps != 200 {
		t.Fatalf("average %+v", o)
	}
}

func TestDeviceHistoryStoredFoldedAndServed(t *testing.T) {
	h := newHarness(t)
	s := h.srv
	ctx := context.Background()
	dev := "dev1"
	base := time.Now().UTC().Truncate(time.Hour).Add(-3 * time.Hour)
	// two hours of 5 s samples with a CPU value that depends on the minute
	for i := 0; i < 2*3600/5; i++ {
		ts := base.Add(time.Duration(i) * 5 * time.Second)
		s.recordHistory(ctx, dev, MetricSample{Time: ts, CPUPercent: float64(ts.Minute()), MemPercent: 50})
	}
	min1, err := s.storedHistory(ctx, dev, 60, 4*time.Hour)
	if err != nil || len(min1) < 119 || len(min1) > 120 {
		t.Fatalf("1-minute rows %d err %v", len(min1), err)
	}
	min5, _ := s.storedHistory(ctx, dev, 300, 4*time.Hour)
	if len(min5) < 22 || len(min5) > 24 {
		t.Fatalf("5-minute rows %d", len(min5))
	}
	hourly, _ := s.storedHistory(ctx, dev, 3600, 4*time.Hour)
	if len(hourly) != 1 || hourly[0].MemPercent != 50 {
		t.Fatalf("hourly rows %+v", hourly)
	}
	// 5-minute point averaging minutes 0-4 is 2
	if min5[0].CPUPercent != 2 {
		t.Fatalf("first 5-minute point %v", min5[0].CPUPercent)
	}
	// a restart refills the live hour
	s.metrics = newMetricsHistory(720)
	s.metrics.prime(dev, min1[len(min1)-5:])
	if got := s.metrics.get(dev); len(got) != 5 {
		t.Fatalf("primed %d", len(got))
	}
	// pruning respects retention
	s.setJSON(ctx, settingDeviceMetrics, deviceMetricsSettings{RetentionDays: 1})
	if s.deviceRetention(ctx) != 1 {
		t.Fatal("retention not saved")
	}
	old := time.Now().Add(-30 * time.Hour)
	if err := s.store.PutDeviceSample(ctx, dev, 60, old.Unix(), []byte(`{"time":"2026-01-01T00:00:00Z"}`)); err != nil {
		t.Fatal(err)
	}
	s.pruneDeviceHistory(ctx)
	if rows, _ := s.store.DeviceSamples(ctx, dev, 60, 0); len(rows) > 0 {
		for _, r := range rows {
			if string(r) == `{"time":"2026-01-01T00:00:00Z"}` {
				t.Fatal("old sample survived")
			}
		}
	}
}

func TestRangeTier(t *testing.T) {
	for r, want := range map[string]int{"": 0, "1h": 0, "6h": 60, "24h": 60, "7d": 300, "30d": 3600, "90d": 3600} {
		if res, _, ok := rangeTier(r); !ok || res != want {
			t.Errorf("%q -> %d %v", r, res, ok)
		}
	}
	if _, _, ok := rangeTier("2y"); ok {
		t.Error("2y accepted")
	}
}
