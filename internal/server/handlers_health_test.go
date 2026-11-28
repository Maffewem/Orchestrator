package server

import (
	"context"
	"strings"
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

func TestTemperatureThresholdsAreConfigurable(t *testing.T) {
	st, _ := store.Open(":memory:")
	s := &Server{store: st, cfg: Config{HeartbeatInterval: time.Second}}
	ctx := context.Background()
	if w, c := s.tempThresholds(ctx); w != 80 || c != 85 {
		t.Fatalf("defaults %v %v", w, c)
	}
	s.store.SetSetting(ctx, settingTempWarn, "60")
	s.store.SetSetting(ctx, settingTempCrit, "70")
	d := &store.Device{LastSeen: time.Now(), Metrics: api.Metrics{TempC: 65}}
	d.Host.Docker = &api.DockerInfo{}
	if v := s.view(d); v.Health != "warning" {
		t.Fatalf("health %q, want warning (%v)", v.Health, v.Issues)
	}
	d.Metrics.TempC = 71
	if v := s.view(d); v.Health != "critical" {
		t.Fatalf("health %q, want critical", v.Health)
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

func TestOOMKilledLoopNamesMemoryLimit(t *testing.T) {
	s := &Server{cfg: Config{HeartbeatInterval: time.Second}, wstats: &workerStats{workers: map[string]*workerAcc{}}}
	now := time.Now().Unix()
	s.wstats.workers["d1|paperless"] = &workerAcc{restarts: []int64{now - 90, now - 60, now - 30}, latest: api.ContainerStats{OOMKilled: true}}
	d := &store.Device{ID: "d1", LastSeen: time.Now(), Containers: []api.Container{{Name: "paperless", State: "running"}}}
	d.Host.CPUs = 4
	d.Host.Docker = &api.DockerInfo{}
	v := s.view(d)
	if len(v.Issues) != 1 || !strings.Contains(v.Issues[0], "out of memory") || !strings.Contains(v.Issues[0], "memory limit") {
		t.Fatalf("issues %v", v.Issues)
	}
}

func killedDevice(status string) *store.Device {
	d := &store.Device{ID: "d1", LastSeen: time.Now(), Containers: []api.Container{{ID: "c1", Name: "paperless", Image: "ghcr.io/paperless-ngx/paperless-ngx", State: "exited", Status: status}}}
	d.Host.CPUs = 4
	d.Host.Docker = &api.DockerInfo{}
	d.Metrics = api.Metrics{MemoryTotal: 4 << 30, MemoryUsed: 3000 << 20}
	return d
}

func TestExit137ExplainedAndAcknowledged(t *testing.T) {
	s := &Server{cfg: Config{HeartbeatInterval: time.Second}, wstats: &workerStats{workers: map[string]*workerAcc{}}}
	s.wstats.workers["d1|paperless"] = &workerAcc{latest: api.ContainerStats{MemLimit: 1 << 30, MemUsed: 900 << 20}}
	d := killedDevice("Exited (137) 2 minutes ago")
	v := s.view(d)
	if v.Health != "warning" || len(v.ContainerIssues) != 1 {
		t.Fatalf("health %q issues %+v", v.Health, v.ContainerIssues)
	}
	ci := v.ContainerIssues[0]
	if ci.SuggestMB != 2048 || ci.SuggestWhy == "" || ci.MemLimit != 1<<30 || !strings.Contains(ci.Cause, "out-of-memory") || !strings.Contains(ci.Cause, "swap") {
		t.Fatalf("issue %+v", ci)
	}
	s.dismiss(context.Background(), "d1", "c1", "paperless", "ack", "Matthew")
	v = s.view(d)
	if v.Health != "ok" || len(v.ContainerIssues) != 1 || !v.ContainerIssues[0].Acked {
		t.Fatalf("after ack: health %q %+v", v.Health, v.ContainerIssues)
	}
	// Running again clears the acknowledgement, so the next kill alerts afresh.
	d.Containers[0].State = "running"
	s.view(d)
	d.Containers[0].State = "exited"
	if v = s.view(d); v.Health != "warning" {
		t.Fatalf("a new kill should alert again, got %q", v.Health)
	}
}

func TestExit137FromRequestedStopIsNotAnAlert(t *testing.T) {
	s := &Server{cfg: Config{HeartbeatInterval: time.Second}}
	d := killedDevice("Exited (137) 5 seconds ago")
	s.markStopped(context.Background(), "d1", d.Containers[0])
	if v := s.view(d); v.Health != "ok" || len(v.ContainerIssues) != 0 {
		t.Fatalf("health %q issues %+v", v.Health, v.ContainerIssues)
	}
}

func TestExit137ClassifiedFromDockersOwnRecord(t *testing.T) {
	s := &Server{cfg: Config{HeartbeatInterval: time.Second}, wstats: &workerStats{workers: map[string]*workerAcc{}}}
	status := "Exited (137) 2 minutes ago"

	// A stop logged just before it died (docker stop on the device that timed out): not an alert.
	d := killedDevice(status)
	d.Containers[0].ExitInspected, d.Containers[0].StopRequested = true, true
	if v := s.view(d); v.Health != "ok" || len(v.ContainerIssues) != 0 {
		t.Fatalf("manual stop: health %q issues %+v", v.Health, v.ContainerIssues)
	}

	// Docker's OOMKilled flag is the proof of an out-of-memory kill, even with no sampled stats.
	d = killedDevice(status)
	d.Containers[0].ExitInspected, d.Containers[0].OOMKilled = true, true
	v := s.view(d)
	if v.Health != "warning" || len(v.ContainerIssues) != 1 || v.ContainerIssues[0].Kind != "oom" {
		t.Fatalf("oom: health %q issues %+v", v.Health, v.ContainerIssues)
	}

	// Neither flag nor a logged stop: listed with an explanation, but not a device warning, and no
	// memory advice.
	d = killedDevice(status)
	d.Containers[0].ExitInspected = true
	v = s.view(d)
	if v.Health != "ok" || len(v.ContainerIssues) != 1 || v.ContainerIssues[0].SuggestMB != 0 || v.ContainerIssues[0].SuggestWhy != "" || strings.Contains(v.ContainerIssues[0].Cause, "RAM") {
		t.Fatalf("unexplained 137: health %q issues %+v", v.Health, v.ContainerIssues)
	}
}

func TestMemoryLimitSuggestionSaysWhatSettingItIs(t *testing.T) {
	why := suggestWhy(2048, 1<<30, 900<<20, 4<<30, 0)
	for _, want := range []string{"memory limit in the project settings", "not the device's RAM or swap", "1.0 GiB now", "no swap"} {
		if !strings.Contains(why, want) {
			t.Errorf("missing %q in %q", want, why)
		}
	}
	if suggestMemoryMB(0, 0, 4<<30) != 0 || !strings.Contains(suggestWhy(0, 0, 0, 4<<30, 0), "no memory limit") {
		t.Error("no limit set: nothing to raise")
	}
}
