package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/Maffewem/Orchestrator/internal/api"
	"github.com/Maffewem/Orchestrator/internal/store"
)

// A setup plan is what the Add device wizard collects before the device exists: which extras to
// switch on once it joins. The plan is kept on the server and the install command only carries its
// ID, so secrets such as a tunnel token never appear in shell history.

const deviceSetupPrefix = "devsetup:"

type deviceSetup struct {
	// Proxy starts the Caddy proxy that serves domains.
	Proxy bool `json:"proxy"`
	// TunnelToken sets up a Cloudflare Tunnel; TunnelVia is "" (direct) or "proxy".
	TunnelToken string `json:"tunnel_token,omitempty"`
	TunnelVia   string `json:"tunnel_via,omitempty"`
	// Maintenance keeps new workers off the device until it is released.
	Maintenance     bool   `json:"maintenance"`
	MaintenanceNote string `json:"maintenance_note,omitempty"`
}

func (s *Server) handleCreateDeviceSetup(w http.ResponseWriter, r *http.Request) {
	var req deviceSetup
	if !decode(w, r, &req) {
		return
	}
	req.TunnelToken = strings.TrimSpace(req.TunnelToken)
	if f := strings.Fields(req.TunnelToken); len(f) > 1 {
		req.TunnelToken = f[len(f)-1]
	}
	if req.TunnelToken != "" {
		if _, err := parseTunnelToken(req.TunnelToken); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	if req.TunnelVia != "" && req.TunnelVia != "proxy" {
		writeError(w, http.StatusBadRequest, "tunnel_via must be empty (direct) or proxy")
		return
	}
	if req.TunnelVia == "proxy" {
		req.Proxy = true // the tunnel sends traffic to Caddy, so it must run
	}
	req.MaintenanceNote = strings.TrimSpace(req.MaintenanceNote)
	b := make([]byte, 8)
	rand.Read(b)
	id := hex.EncodeToString(b)
	v, _ := json.Marshal(req)
	if err := s.store.SetSetting(r.Context(), deviceSetupPrefix+id, string(v)); err != nil {
		s.internal(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"id": id})
}

// applyDeviceSetup runs a plan for a device that has just registered. The plan is single use.
// Problems are reported as events and never stop registration.
func (s *Server) applyDeviceSetup(ctx context.Context, d *store.Device, id string) {
	if id == "" || strings.ContainsAny(id, " :/") {
		return
	}
	key := deviceSetupPrefix + id
	raw, _ := s.store.GetSetting(ctx, key)
	if raw == "" {
		return
	}
	s.store.SetSetting(ctx, key, "")
	var p deviceSetup
	if json.Unmarshal([]byte(raw), &p) != nil {
		return
	}
	warn := func(what string, err error) {
		s.event(ctx, d.ID, "warning", "device.setup", "Setup step \""+what+"\" failed on "+d.Name+": "+err.Error())
	}
	if p.Maintenance {
		note := p.MaintenanceNote
		if note == "" {
			note = "Set up in maintenance mode"
		}
		if err := s.store.SetDeviceMaintenance(ctx, d.ID, true, note, time.Time{}); err != nil {
			warn("maintenance", err)
		}
	}
	if p.Proxy {
		if err := s.pushProxy(ctx, d.ID, true); err != nil {
			warn("proxy", err)
		}
	}
	if p.TunnelToken != "" {
		cfg := tunnelConfig{Token: p.TunnelToken, Via: p.TunnelVia}
		if err := s.saveTunnelConfig(ctx, d.ID, &cfg); err != nil {
			warn("tunnel", err)
		} else if _, err := s.store.CreateCommand(ctx, d.ID, api.ActionTunnelSync, tunnelContainer, map[string]string{"token": p.TunnelToken}); err != nil {
			warn("tunnel", err)
		}
	}
	s.event(ctx, d.ID, "info", "device.setup", "Applied the setup plan chosen in the wizard to "+d.Name)
}
