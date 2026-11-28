package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Maffewem/Orchestrator/internal/api"
	"github.com/Maffewem/Orchestrator/internal/store"
)

// Graceful shutdown and restart of a whole device. Only available while the device is in
// maintenance mode, so nothing new is being placed on it and the operator has already decided
// to take it out of service. Admin only; the device name must be typed back.

const settingPowerPrefix = "power:"

// powerPending remembers a shutdown or restart that was asked for, so the dashboard can show
// "Shutting down" / "Restarting" and finish the job when the device returns.
type powerPending struct {
	Mode string    `json:"mode"` // shutdown | restart
	By   string    `json:"by"`
	At   time.Time `json:"at"`
	// ExitMaintenance takes the device out of maintenance mode once it is back (restart, or power-on after a shutdown).
	ExitMaintenance bool `json:"exit_maintenance,omitempty"`
}

// PowerImpact is what taking a device down affects, shown before the person confirms.
type PowerImpact struct {
	HostsServer     bool `json:"hosts_server"`     // the Orchestrator server itself runs on this device
	RunningWorkers  int  `json:"running_workers"`  // containers running now
	FailoverWorkers int  `json:"failover_workers"` // workers with a failover policy: they will NOT move, maintenance devices are skipped
	ClusterEnabled  bool `json:"cluster_enabled"`
	// HAPaired is set on the server host when a standby server exists: it is promoted by hand, never automatically.
	HAPaired bool `json:"ha_paired"`
}

func (s *Server) loadPower(ctx context.Context, id string) *powerPending {
	if s.store == nil {
		return nil
	}
	raw, err := s.store.GetSetting(ctx, settingPowerPrefix+id)
	if err != nil || raw == "" {
		return nil
	}
	var p powerPending
	if json.Unmarshal([]byte(raw), &p) != nil || p.Mode == "" {
		return nil
	}
	return &p
}

func (s *Server) savePower(ctx context.Context, id string, p *powerPending) {
	v := ""
	if p != nil {
		b, _ := json.Marshal(p)
		v = string(b)
	}
	if err := s.store.SetSetting(ctx, settingPowerPrefix+id, v); err != nil {
		s.log.Error("save power state", "err", err)
	}
}

func (s *Server) powerImpact(ctx context.Context, d *store.Device) PowerImpact {
	imp := PowerImpact{ClusterEnabled: s.clusterConfig(ctx).Enabled}
	if devs, err := s.store.ListDevices(ctx); err == nil {
		if h := serverHostDevice(devs); h != nil && h.ID == d.ID {
			imp.HostsServer = true
			imp.HAPaired = s.loadHA(ctx).PeerURL != ""
		}
	}
	for _, c := range d.Containers {
		if c.State == "running" {
			imp.RunningWorkers++
		}
	}
	if imp.ClusterEnabled {
		if projects, err := s.store.ListProjects(ctx); err == nil {
			for _, p := range projects {
				if p.DeviceID == d.ID && s.clusterPolicy(ctx, p.ID).Failover {
					imp.FailoverWorkers++
				}
			}
		}
	}
	return imp
}

// handleDevicePower asks a device in maintenance mode to shut down or restart.
func (s *Server) handleDevicePower(w http.ResponseWriter, r *http.Request) {
	d, ok := s.lookupDevice(w, r)
	if !ok {
		return
	}
	var req struct {
		Mode            string `json:"mode"` // shutdown | restart
		Confirm         string `json:"confirm"`
		ExitMaintenance bool   `json:"exit_maintenance"`
	}
	if !decode(w, r, &req) {
		return
	}
	if req.Mode != "shutdown" && req.Mode != "restart" {
		writeError(w, http.StatusBadRequest, "mode must be shutdown or restart")
		return
	}
	if strings.TrimSpace(req.Confirm) != d.Name {
		writeError(w, http.StatusBadRequest, "type the device name ("+d.Name+") to confirm")
		return
	}
	if !d.Maintenance {
		writeError(w, http.StatusConflict, "put "+d.Name+" in maintenance mode first, so nothing new is placed on it while it is down")
		return
	}
	if !hasCap(d, api.CapPower) {
		writeError(w, http.StatusConflict, "this device's agent is too old to shut down or restart the machine; update the agent from the Updates page first")
		return
	}
	if d.Offline || s.view(d).Status != "online" {
		writeError(w, http.StatusConflict, d.Name+" is offline")
		return
	}
	if p := s.loadPower(r.Context(), d.ID); p != nil && time.Since(p.At) < 15*time.Minute {
		writeError(w, http.StatusConflict, "a "+p.Mode+" is already in progress on "+d.Name)
		return
	}
	who := "an admin"
	if i := identityFrom(r); i != nil {
		who = i.Name
	}
	mode := map[string]string{"shutdown": "poweroff", "restart": "reboot"}[req.Mode]
	cmd, err := s.store.CreateCommand(r.Context(), d.ID, api.ActionDevicePower, "", map[string]string{"mode": mode, "confirm": mode, "by": who})
	if err != nil {
		s.internal(w, err)
		return
	}
	s.savePower(r.Context(), d.ID, &powerPending{Mode: req.Mode, By: who, At: time.Now(), ExitMaintenance: req.ExitMaintenance})
	verb := map[string]string{"shutdown": "shut down", "restart": "restart"}[req.Mode]
	s.event(r.Context(), d.ID, "warning", "device.power.requested", fmt.Sprintf("%s asked %s to %s", who, d.Name, verb))
	writeJSON(w, http.StatusAccepted, cmd)
}

// powerResult records the outcome of a device.power command. A failure means nothing was stopped.
func (s *Server) powerResult(ctx context.Context, d *store.Device, c *api.Command, res api.CommandResult) {
	if res.OK {
		s.event(ctx, d.ID, "info", api.ActionDevicePower, d.Name+" accepted the request and is stopping its containers")
		return
	}
	s.savePower(ctx, d.ID, nil)
	s.event(ctx, d.ID, "error", api.ActionDevicePower, fmt.Sprintf("%s could not %s: %s", d.Name, map[string]string{"poweroff": "shut down", "reboot": "restart"}[c.Args["mode"]], res.Error))
}

// powerReturned runs on every heartbeat. A device that booted after the request has come back:
// the pending marker is cleared and maintenance mode is left when that was asked for.
func (s *Server) powerReturned(ctx context.Context, d *store.Device, m api.Metrics) {
	p := s.loadPower(ctx, d.ID)
	if p == nil {
		return
	}
	since := time.Since(p.At)
	if since < 5*time.Second || time.Duration(m.UptimeSeconds)*time.Second > since {
		return // still the machine that was asked to stop
	}
	s.savePower(ctx, d.ID, nil)
	s.event(ctx, d.ID, "info", "device.power.returned", d.Name+" is back after a "+p.Mode)
	if p.ExitMaintenance && d.Maintenance {
		if err := s.store.SetDeviceMaintenance(ctx, d.ID, false, "", time.Time{}); err == nil {
			s.event(ctx, d.ID, "info", "device.maintenance", d.Name+" left maintenance mode (it came back from a "+p.Mode+")")
		}
	}
}
