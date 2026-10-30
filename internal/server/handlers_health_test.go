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
