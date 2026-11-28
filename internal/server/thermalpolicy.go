package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Maffewem/Orchestrator/internal/api"
)

// Per-device fan control and thermal protection. The policy is stored here and sent to the
// agent on every heartbeat; the agent applies it locally (internal/agent/thermal.go) and
// reports what it did, which this file turns into Activity entries and Needs attention items.

// minFanPct is the lowest duty a policy may ask for: a fan that is nearly stopped is not safe.
const minFanPct = 20

type thermalStatuses struct {
	mu sync.Mutex
	m  map[string]*api.ThermalStatus
}

func (t *thermalStatuses) set(id string, st *api.ThermalStatus) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.m == nil {
		t.m = map[string]*api.ThermalStatus{}
	}
	c := *st
	c.Events = nil
	t.m[id] = &c
}

func (t *thermalStatuses) get(id string) *api.ThermalStatus {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.m[id]
}

func thermalKey(deviceID string) string { return "thermal:" + deviceID }

func (s *Server) thermalPolicy(ctx context.Context, deviceID string) (api.ThermalPolicy, bool) {
	var p api.ThermalPolicy
	v, err := s.store.GetSetting(ctx, thermalKey(deviceID))
	if err != nil || v == "" || json.Unmarshal([]byte(v), &p) != nil {
		return api.ThermalPolicy{FanMode: "auto", MinPct: 30}, false
	}
	return p, true
}

// policyForAgent is the stored policy plus the server's critical temperature.
func (s *Server) policyForAgent(ctx context.Context, deviceID string) *api.ThermalPolicy {
	p, ok := s.thermalPolicy(ctx, deviceID)
	if !ok {
		return nil
	}
	_, p.CritC = s.tempThresholds(ctx)
	return &p
}

// ingestThermal records the agent's thermal report and logs what it did on its own.
func (s *Server) ingestThermal(ctx context.Context, deviceID string, st *api.ThermalStatus) {
	if st == nil {
		return
	}
	for _, e := range st.Events {
		s.event(ctx, deviceID, e.Level, "thermal.action", e.Message)
	}
	s.thermal.set(deviceID, st)
}

type thermalView struct {
	Policy api.ThermalPolicy `json:"policy"`
	Saved  bool              `json:"saved"`
	// Supported is false until the device runs an agent that has the thermal controller.
	Supported bool               `json:"supported"`
	Status    *api.ThermalStatus `json:"status,omitempty"`
	WarnC     float64            `json:"warn_c"`
	CritC     float64            `json:"crit_c"`
	MinPct    int                `json:"min_pct"`
}

func (s *Server) handleGetThermal(w http.ResponseWriter, r *http.Request) {
	d, ok := s.lookupDevice(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	p, saved := s.thermalPolicy(ctx, d.ID)
	warn, crit := s.tempThresholds(ctx)
	writeJSON(w, http.StatusOK, thermalView{Policy: p, Saved: saved, Supported: hasCap(d, api.CapThermal), Status: s.thermal.get(d.ID), WarnC: warn, CritC: crit, MinPct: minFanPct})
}

// validateThermal checks a policy before it is saved; it returns the cleaned policy.
func validateThermal(p api.ThermalPolicy, crit float64) (api.ThermalPolicy, error) {
	switch p.FanMode {
	case "auto", "manual", "curve":
	default:
		return p, fmt.Errorf("fan mode must be auto, manual or curve")
	}
	if p.MinPct < minFanPct || p.MinPct > 100 {
		return p, fmt.Errorf("the minimum fan speed must be between %d%% and 100%%", minFanPct)
	}
	if p.FanManual < 0 || p.FanManual > 100 {
		return p, fmt.Errorf("manual fan speed must be between 0%% and 100%%")
	}
	if p.FanMode == "curve" {
		if len(p.Curve) < 2 || len(p.Curve) > 10 {
			return p, fmt.Errorf("a curve needs 2 to 10 points")
		}
		for i, c := range p.Curve {
			if c.C < 20 || c.C > 120 || c.Pct < 0 || c.Pct > 100 {
				return p, fmt.Errorf("curve point %d is out of range (20-120°C, 0-100%%)", i+1)
			}
			if i > 0 && (c.C <= p.Curve[i-1].C || c.Pct < p.Curve[i-1].Pct) {
				return p, fmt.Errorf("curve points must rise in temperature, and the fan speed must not fall")
			}
		}
		last := p.Curve[len(p.Curve)-1]
		if last.Pct < 100 || last.C > crit {
			return p, fmt.Errorf("the curve must reach 100%% by the critical temperature (%.0f°C)", crit)
		}
	}
	pr := &p.Protect
	if pr.Enabled {
		if pr.TempC < 50 || pr.TempC > 120 {
			return p, fmt.Errorf("the protection temperature must be between 50°C and 120°C")
		}
		if pr.ForSeconds < 30 || pr.ForSeconds > 3600 {
			return p, fmt.Errorf("the temperature must be sustained for 30 to 3600 seconds")
		}
		if pr.ResumeBelowC < 30 || pr.ResumeBelowC > pr.TempC-3 {
			return p, fmt.Errorf("resume below must be at least 3°C under the protection temperature (and at least 30°C)")
		}
		if !pr.FanFull && len(pr.Stop) == 0 {
			return p, fmt.Errorf("choose at least one action: full fan speed or workers to stop")
		}
	}
	clean := func(in []string) []string {
		var out []string
		for _, n := range in {
			if n = strings.TrimSpace(n); n != "" && !slices.Contains(out, n) {
				out = append(out, n)
			}
		}
		return out
	}
	pr.Stop = clean(pr.Stop)
	p.Critical = clean(p.Critical)
	return p, nil
}

func (s *Server) handleSetThermal(w http.ResponseWriter, r *http.Request) {
	d, ok := s.lookupDevice(w, r)
	if !ok {
		return
	}
	var req api.ThermalPolicy
	if !decode(w, r, &req) {
		return
	}
	ctx := r.Context()
	_, crit := s.tempThresholds(ctx)
	p, err := validateThermal(req, crit)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	p.Rev = time.Now().UnixNano()
	p.CritC = 0
	b, _ := json.Marshal(p)
	if err := s.store.SetSetting(ctx, thermalKey(d.ID), string(b)); err != nil {
		s.internal(w, err)
		return
	}
	s.event(ctx, d.ID, "info", "settings.thermal", fmt.Sprintf("Thermal policy for %s updated (fan %s, protection %s)", d.Name, p.FanMode, map[bool]string{true: "on", false: "off"}[p.Protect.Enabled]))
	s.handleGetThermal(w, r)
}

// thermalIssues are the device issues the agent's thermal controller raises.
func (s *Server) thermalIssues(id string) (critical, warning []string) {
	st := s.thermal.get(id)
	if st == nil {
		return
	}
	if st.Protecting {
		msg := "Thermal protection is active"
		if len(st.Stopped) > 0 {
			msg += "; stopped " + strings.Join(st.Stopped, ", ")
		}
		critical = append(critical, msg)
	}
	if st.Failsafe != "" {
		warning = append(warning, "Fan forced to full speed: "+st.Failsafe)
	}
	return
}
