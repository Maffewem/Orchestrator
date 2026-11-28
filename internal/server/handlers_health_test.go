package server

import (
	"testing"
	"time"

	"github.com/Maffewem/Orchestrator/internal/api"
	"github.com/Maffewem/Orchestrator/internal/store"
)

func TestDeviceViewHealthChecks(t *testing.T) {
	s := &Server{cfg: Config{HeartbeatInterval: time.Second}}
	mk := func(m api.Metrics, cs ...api.Container) DeviceView {
		d := &store.Device{LastSeen: time.Now(), Metrics: m, Containers: cs}
		d.Host.CPUs = 4
		d.Host.Docker = &api.DockerInfo{}
		return s.view(d)
	}
	cases := []struct {
		name   string
		v      DeviceView
		health string
	}{
		{"ok", mk(api.Metrics{Load5: 2, TempC: 60}), "ok"},
		{"load warning", mk(api.Metrics{Load5: 6}), "warning"},
		{"load critical", mk(api.Metrics{Load5: 12}), "critical"},
		{"hot", mk(api.Metrics{TempC: 81}), "warning"},
		{"too hot", mk(api.Metrics{TempC: 86}), "critical"},
		{"crash loop", mk(api.Metrics{}, api.Container{Name: "a", State: "restarting"}), "warning"},
		{"oom kill", mk(api.Metrics{}, api.Container{Name: "a", State: "exited", Status: "Exited (137) 1 minute ago"}), "warning"},
		{"app error ignored", mk(api.Metrics{}, api.Container{Name: "a", State: "exited", Status: "Exited (1) 1 minute ago"}), "ok"},
	}
	for _, c := range cases {
		if c.v.Health != c.health {
			t.Errorf("%s: health %q, want %q (%v)", c.name, c.v.Health, c.health, c.v.Issues)
		}
	}
}

func TestNetworkNoticeStaysUntilAcknowledged(t *testing.T) {
	s := &Server{cfg: Config{HeartbeatInterval: time.Second}}
	d := &store.Device{ID: "d1", LastSeen: time.Now()}
	d.Host.Docker = &api.DockerInfo{}
	s.setNetNote(t.Context(), d.ID, "IP address changed from 10.0.0.2 to 10.0.0.9")
	// Still shown long after the old six-hour expiry would have passed.
	s.netNotes.m[d.ID] = NetNotice{Message: s.netNotes.m[d.ID].Message, At: time.Now().Add(-48 * time.Hour)}
	v := s.view(d)
	if v.NetworkNotice == nil || v.Health != "warning" {
		t.Fatalf("notice should persist unacknowledged: %+v health=%s", v.NetworkNotice, v.Health)
	}
	if !s.ackNetNote(t.Context(), d.ID) {
		t.Fatal("ack reported no notice")
	}
	if v := s.view(d); v.NetworkNotice != nil || v.Health != "ok" {
		t.Fatalf("acknowledged notice should clear: %+v health=%s", v.NetworkNotice, v.Health)
	}
	if s.ackNetNote(t.Context(), d.ID) {
		t.Fatal("second ack should find nothing")
	}
	s.setNetNote(t.Context(), d.ID, "Gained IP address 10.0.0.10")
	if v := s.view(d); v.NetworkNotice == nil {
		t.Fatal("a new change should raise a new notice")
	}
}

func TestRestartLoopMarksContainerUnstable(t *testing.T) {
	s := &Server{cfg: Config{HeartbeatInterval: time.Second}, wstats: &workerStats{workers: map[string]*workerAcc{}}}
	now := time.Now().Unix()
	s.wstats.workers["d1|paperless"] = &workerAcc{restarts: []int64{now - 90, now - 60, now - 30}}
	s.wstats.workers["d1|old"] = &workerAcc{restarts: []int64{now - 4000, now - 3900, now - 3800}}
	d := &store.Device{ID: "d1", LastSeen: time.Now(), Containers: []api.Container{
		{Name: "paperless", State: "running"}, {Name: "old", State: "running"}, {Name: "calm", State: "running"},
	}}
	d.Host.CPUs = 4
	d.Host.Docker = &api.DockerInfo{}
	v := s.view(d)
	if v.Health != "warning" || len(v.Issues) != 1 {
		t.Fatalf("health %q issues %v", v.Health, v.Issues)
	}
	for _, c := range v.Containers {
		if c.Unstable != (c.Name == "paperless") {
			t.Errorf("%s unstable=%v", c.Name, c.Unstable)
		}
	}
	if d.Containers[0].Unstable {
		t.Error("stored device was modified")
	}
}
