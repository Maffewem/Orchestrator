package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Maffewem/Orchestrator/internal/api"
	"github.com/Maffewem/Orchestrator/internal/store"
)

// Keeping cloudflared current. The agent pulls cloudflare/cloudflared:latest only when it creates
// the container, and an existing container is merely restarted, so a tunnel never moves past the
// version it started with. Updating pulls the newest image and recreates the container with the
// same token and settings; it can be done by hand or, when switched on for a device, by the server.

const (
	cloudflaredReleaseURL = "https://api.github.com/repos/cloudflare/cloudflared/releases/latest"
	latestTTL             = 6 * time.Hour
	latestRetry           = 10 * time.Minute
	// autoUpdateRetry spaces automatic attempts on one device, so a failing update is not repeated every pass.
	autoUpdateRetry = 6 * time.Hour
)

// parseVersion turns "2025.11.1" (or "v2025.11.1") into its numeric parts.
func parseVersion(v string) []int {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if v == "" {
		return nil
	}
	var out []int
	for _, p := range strings.Split(v, ".") {
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil
		}
		out = append(out, n)
	}
	return out
}

// versionOlder reports whether have is older than latest. Unknown versions are never "older".
func versionOlder(have, latest string) bool {
	a, b := parseVersion(have), parseVersion(latest)
	if a == nil || b == nil {
		return false
	}
	for i := 0; i < len(a) || i < len(b); i++ {
		var x, y int
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		if x != y {
			return x < y
		}
	}
	return false
}

// cloudflaredLatest returns the newest cloudflared release the server has seen ("" when unknown).
// With refresh it looks again when the answer is older than six hours, in the background, so a
// slow or blocked GitHub never holds up a dashboard request.
func (s *Server) cloudflaredLatest(ctx context.Context, refresh bool) string {
	s.cfdMu.Lock()
	latest, tried := s.cfdLatest, s.cfdTried
	ttl := latestTTL
	if latest == "" {
		ttl = latestRetry
	}
	stale := time.Since(tried) > ttl
	if refresh && stale {
		s.cfdTried = time.Now() // claim the lookup so concurrent callers don't pile on
	}
	s.cfdMu.Unlock()
	if refresh && stale {
		go func() {
			v, err := s.fetchCloudflaredLatest()
			if err != nil {
				s.log.Debug("cloudflared release lookup failed", "err", err)
				return
			}
			s.cfdMu.Lock()
			s.cfdLatest = v
			s.cfdMu.Unlock()
		}()
	}
	return latest
}

// freshCloudflaredLatest is cloudflaredLatest that waits for the lookup when the cached answer is stale.
func (s *Server) freshCloudflaredLatest() string {
	s.cfdMu.Lock()
	latest, tried := s.cfdLatest, s.cfdTried
	s.cfdMu.Unlock()
	if latest != "" && time.Since(tried) < latestTTL {
		return latest
	}
	v, err := s.fetchCloudflaredLatest()
	if err != nil {
		s.log.Debug("cloudflared release lookup failed", "err", err)
		return latest // an older answer beats none
	}
	s.cfdMu.Lock()
	s.cfdLatest, s.cfdTried = v, time.Now()
	s.cfdMu.Unlock()
	return v
}

func (s *Server) fetchCloudflaredLatest() (string, error) {
	u := s.cfdURL
	if u == "" {
		u = cloudflaredReleaseURL
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "orchestrator")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("release lookup: %s", resp.Status)
	}
	var rel struct {
		Tag string `json:"tag_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return "", err
	}
	if parseVersion(rel.Tag) == nil {
		return "", fmt.Errorf("unexpected release tag %q", rel.Tag)
	}
	return strings.TrimPrefix(rel.Tag, "v"), nil
}

// queueTunnelUpdate asks the agent to pull the newest cloudflared and recreate the container.
func (s *Server) queueTunnelUpdate(ctx context.Context, d *store.Device, cfg tunnelConfig) error {
	_, err := s.store.CreateCommand(ctx, d.ID, api.ActionTunnelSync, tunnelContainer, map[string]string{"token": cfg.Token, "update": "1"})
	return err
}

// handleUpdateTunnel updates cloudflared on the device now. The tunnel drops for a few seconds
// while the container is recreated.
func (s *Server) handleUpdateTunnel(w http.ResponseWriter, r *http.Request) {
	d, ok := s.lookupDevice(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	cfg, has := s.tunnelConfig(ctx, d.ID)
	if !has {
		writeError(w, http.StatusConflict, "this device has no Cloudflare Tunnel")
		return
	}
	if !hasCap(d, api.CapTunnelUpdate) {
		writeError(w, http.StatusConflict, "this device's agent is too old to update the tunnel; update the agent from the Updates page first")
		return
	}
	if err := s.queueTunnelUpdate(ctx, d, cfg); err != nil {
		s.internal(w, err)
		return
	}
	s.event(ctx, d.ID, "info", "tunnel.update", "Updating cloudflared on "+d.Name)
	writeJSON(w, http.StatusAccepted, s.tunnelView(ctx, d))
}

func (s *Server) handleSetTunnelAutoUpdate(w http.ResponseWriter, r *http.Request) {
	d, ok := s.lookupDevice(w, r)
	if !ok {
		return
	}
	var req struct {
		Enabled bool `json:"enabled"`
	}
	if !decode(w, r, &req) {
		return
	}
	ctx := r.Context()
	cfg, has := s.tunnelConfig(ctx, d.ID)
	if !has {
		writeError(w, http.StatusConflict, "this device has no Cloudflare Tunnel")
		return
	}
	cfg.AutoUpdate = req.Enabled
	if err := s.saveTunnelConfig(ctx, d.ID, &cfg); err != nil {
		s.internal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.tunnelView(ctx, d))
}

func tunnelAutoKey(deviceID string) string { return "tunnel-auto-update:" + deviceID }

// tunnelUpdatePass runs when the Orchestrator update schedule fires (see scheduleTick), so
// unattended changes all land in the window the admin chose. It queues an update for each
// opted-in device that is online, not in maintenance mode, reports a version older than the latest release and has not been tried recently.
func (s *Server) tunnelUpdatePass(ctx context.Context, now time.Time) {
	latest := s.freshCloudflaredLatest()
	if latest == "" {
		return
	}
	devs, err := s.store.ListDevices(ctx)
	if err != nil {
		return
	}
	for _, d := range devs {
		cfg, has := s.tunnelConfig(ctx, d.ID)
		if !has || !cfg.AutoUpdate || d.Maintenance || !hasCap(d, api.CapTunnelUpdate) || s.view(d).Status != "online" {
			continue
		}
		if d.Host.Tunnel == nil || !versionOlder(d.Host.Tunnel.Version, latest) {
			continue
		}
		if v, _ := s.store.GetSetting(ctx, tunnelAutoKey(d.ID)); v != "" {
			if at, err := time.Parse(time.RFC3339, v); err == nil && now.Sub(at) < autoUpdateRetry {
				continue
			}
		}
		s.store.SetSetting(ctx, tunnelAutoKey(d.ID), now.UTC().Format(time.RFC3339))
		if err := s.queueTunnelUpdate(ctx, d, cfg); err != nil {
			s.log.Warn("queue tunnel update", "device", d.Name, "err", err)
			continue
		}
		s.event(ctx, d.ID, "info", "tunnel.update", fmt.Sprintf("Updating cloudflared on %s automatically (%s to %s)", d.Name, d.Host.Tunnel.Version, latest))
	}
}

// tunnelUpdateWhen describes when automatic tunnel updates run: with the Orchestrator update
// schedule, or never while that schedule is off.
func (s *Server) tunnelUpdateWhen(ctx context.Context) string {
	sch := s.loadSchedule(ctx)
	switch sch.Mode {
	case "daily":
		return fmt.Sprintf("daily at %s %s", sch.Time, sch.TZ)
	case "weekly":
		return fmt.Sprintf("every %s at %s %s", time.Weekday(sch.Weekday), sch.Time, sch.TZ)
	}
	return ""
}
