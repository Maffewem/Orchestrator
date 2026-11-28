package server

import (
	"strings"
	"testing"
	"time"

	"github.com/Maffewem/Orchestrator/internal/api"
)

func TestValidateThermal(t *testing.T) {
	good := api.ThermalPolicy{FanMode: "curve", MinPct: 30, Curve: []api.CurvePoint{{C: 40, Pct: 30}, {C: 80, Pct: 100}},
		Protect: api.ThermalProtect{Enabled: true, TempC: 75, ForSeconds: 60, ResumeBelowC: 65, FanFull: true, Stop: []string{" a ", "a", "b"}}, Critical: []string{"c"}}
	p, err := validateThermal(good, 85)
	if err != nil || len(p.Protect.Stop) != 2 {
		t.Fatalf("good policy rejected: %v %v", err, p.Protect.Stop)
	}
	cases := map[string]func(*api.ThermalPolicy){
		"minimum below safe":     func(p *api.ThermalPolicy) { p.MinPct = 10 },
		"curve not reaching 100": func(p *api.ThermalPolicy) { p.Curve[1].Pct = 90 },
		"curve past critical":    func(p *api.ThermalPolicy) { p.Curve[1].C = 90 },
		"falling curve":          func(p *api.ThermalPolicy) { p.Curve = []api.CurvePoint{{C: 40, Pct: 60}, {C: 50, Pct: 40}, {C: 80, Pct: 100}} },
		"no hysteresis":          func(p *api.ThermalPolicy) { p.Protect.ResumeBelowC = 74 },
		"too short":              func(p *api.ThermalPolicy) { p.Protect.ForSeconds = 5 },
		"no action":              func(p *api.ThermalPolicy) { p.Protect.FanFull, p.Protect.Stop = false, nil },
		"bad mode":               func(p *api.ThermalPolicy) { p.FanMode = "turbo" },
	}
	for name, mut := range cases {
		c := good
		c.Curve = append([]api.CurvePoint(nil), good.Curve...)
		mut(&c)
		if _, err := validateThermal(c, 85); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestThermalRoundTrip(t *testing.T) {
	h := newHarness(t)
	reg := h.register("blade-1")
	hb := api.HeartbeatRequest{Host: api.HostInfo{Capabilities: []string{api.CapThermal}}}
	var res api.HeartbeatResponse
	h.do("POST", "/api/agent/heartbeat", reg.AgentToken, hb, &res)
	if res.Thermal != nil {
		t.Fatal("no policy yet")
	}
	pol := api.ThermalPolicy{FanMode: "manual", FanManual: 60, MinPct: 30,
		Protect: api.ThermalProtect{Enabled: true, TempC: 75, ForSeconds: 60, ResumeBelowC: 65, Stop: []string{"batch"}}}
	if code := h.do("PUT", "/api/devices/"+reg.DeviceID+"/thermal", adminToken, pol, nil); code != 200 {
		t.Fatalf("save: %d", code)
	}
	// Only admins may change it.
	if code := h.do("PUT", "/api/devices/"+reg.DeviceID+"/thermal", h.makeUser("operator1", "operator"), pol, nil); code != 403 {
		t.Fatalf("operator got %d", code)
	}
	h.do("POST", "/api/agent/heartbeat", reg.AgentToken, hb, &res)
	if res.Thermal == nil || res.Thermal.FanMode != "manual" || res.Thermal.CritC != 85 || res.Thermal.Rev == 0 {
		t.Fatalf("agent policy: %+v", res.Thermal)
	}
	// The agent reports protection active: it shows as a critical issue and its actions land in Activity.
	hb.Thermal = &api.ThermalStatus{Protecting: true, Stopped: []string{"batch"}, Events: []api.ThermalEvent{{At: time.Now(), Level: "critical", Message: "Stopped worker batch to cool the device"}}}
	h.do("POST", "/api/agent/heartbeat", reg.AgentToken, hb, nil)
	var d struct {
		Health string   `json:"health"`
		Issues []string `json:"issues"`
	}
	h.do("GET", "/api/devices/"+reg.DeviceID, adminToken, nil, &d)
	if d.Health != "critical" || !strings.Contains(strings.Join(d.Issues, "|"), "stopped batch") {
		t.Fatalf("issues: %+v", d)
	}
	var ev []struct{ Kind, Message string }
	h.do("GET", "/api/events?device="+reg.DeviceID, adminToken, nil, &ev)
	found := false
	for _, e := range ev {
		found = found || e.Kind == "thermal.action"
	}
	if !found {
		t.Fatalf("no thermal event in Activity: %+v", ev)
	}
}
