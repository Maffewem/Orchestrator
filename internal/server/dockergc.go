package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/Maffewem/Orchestrator/internal/api"
	"github.com/Maffewem/Orchestrator/internal/store"
)

// Docker image retention. Worker images are built per commit and tagged orchestrator/<worker>:<sha>;
// nothing else removes them. The policy below limits how many are kept after a deploy and can
// prune on a schedule. Rolling back to a version whose image is gone still works for
// GitHub-built workers: the agent rebuilds it from the commit.

const (
	settingDockerGC        = "docker_gc_policy"
	settingDockerGCLastRun = "docker_gc_last_run"
)

// DockerGCPolicy is the fleet-wide retention policy.
type DockerGCPolicy struct {
	Keep       int    `json:"keep"`        // newest images kept per worker after a deploy and a prune (0 = no count limit)
	MaxGB      int    `json:"max_gb"`      // size budget for a device's worker images in GB (0 = none)
	Mode       string `json:"mode"`        // scheduled prune: off | daily | weekly
	Time       string `json:"time"`        // HH:MM in TZ
	Weekday    int    `json:"weekday"`     // 0 = Sunday
	TZ         string `json:"tz"`          // IANA name
	Dangling   bool   `json:"dangling"`    // scheduled prune removes untagged images
	BuildCache bool   `json:"build_cache"` // scheduled prune removes build cache older than CacheDays
	CacheDays  int    `json:"cache_days"`  // age limit for the cache (>= 1)
}

func defaultDockerGCPolicy() DockerGCPolicy {
	return DockerGCPolicy{Keep: 3, Mode: "off", Time: "04:00", TZ: "UTC", Dangling: true, BuildCache: true, CacheDays: 7}
}

func (p DockerGCPolicy) schedule() Schedule {
	return Schedule{Mode: p.Mode, Time: p.Time, Weekday: p.Weekday, TZ: p.TZ}
}

func (p DockerGCPolicy) validate() error {
	if err := p.schedule().validate(); err != nil {
		return err
	}
	if p.Keep < 0 || p.Keep > 100 {
		return errors.New("keep must be between 0 (no limit) and 100")
	}
	if p.MaxGB < 0 || p.MaxGB > 100000 {
		return errors.New("size budget must be between 0 (none) and 100000 GB")
	}
	if p.CacheDays < 1 || p.CacheDays > 365 {
		return errors.New("build cache age must be between 1 and 365 days")
	}
	return nil
}

func (s *Server) dockerGCPolicy(ctx context.Context) DockerGCPolicy {
	pol := defaultDockerGCPolicy()
	if s.store == nil {
		return pol
	}
	if v, err := s.store.GetSetting(ctx, settingDockerGC); err == nil {
		json.Unmarshal([]byte(v), &pol)
	}
	return pol
}

type dockerGCView struct {
	DockerGCPolicy
	NextRun *time.Time `json:"next_run,omitempty"`
}

func (s *Server) dockerGCView(ctx context.Context) dockerGCView {
	v := dockerGCView{DockerGCPolicy: s.dockerGCPolicy(ctx)}
	if next, ok := v.schedule().nextSlot(time.Now()); ok {
		v.NextRun = &next
	}
	return v
}

func (s *Server) handleSetDockerGC(w http.ResponseWriter, r *http.Request) {
	var p DockerGCPolicy
	if !decode(w, r, &p) {
		return
	}
	if err := p.validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	b, _ := json.Marshal(p)
	if err := s.store.SetSetting(r.Context(), settingDockerGC, string(b)); err != nil {
		s.internal(w, err)
		return
	}
	// Enabling a schedule counts from now, so an old slot never fires at once.
	s.store.SetSetting(r.Context(), settingDockerGCLastRun, time.Now().UTC().Format(time.RFC3339))
	s.event(r.Context(), "", "info", "settings.docker-gc", fmt.Sprintf("Image retention updated: keep %d per worker, budget %d GB, schedule %s", p.Keep, p.MaxGB, p.Mode))
	writeJSON(w, http.StatusOK, s.dockerGCView(r.Context()))
}

// applyKeepImages tells the agent how many built images of this worker to keep after a deploy.
func (s *Server) applyKeepImages(ctx context.Context, spec *api.DeploySpec) {
	if spec.Build != nil && spec.Compose == nil {
		spec.KeepImages = s.dockerGCPolicy(ctx).Keep
	}
}

func (p DockerGCPolicy) maxBytes() int64 { return int64(p.MaxGB) << 30 }

// handleDockerReport asks the device what its Docker engine holds. Nothing is cached.
func (s *Server) handleDockerReport(w http.ResponseWriter, r *http.Request) {
	d, ok := s.lookupDevice(w, r)
	if !ok {
		return
	}
	if !hasCap(d, api.CapDockerGC) {
		writeError(w, http.StatusConflict, "this device's agent is too old to report Docker storage; update it from the Updates page first")
		return
	}
	if d.Offline {
		writeError(w, http.StatusConflict, d.Name+" is offline")
		return
	}
	pol := s.dockerGCPolicy(r.Context())
	cmd, err := s.store.CreateCommand(r.Context(), d.ID, api.ActionDockerReport, "", map[string]string{
		"keep": strconv.Itoa(pol.Keep), "max_bytes": strconv.FormatInt(pol.maxBytes(), 10),
	})
	if err != nil {
		s.internal(w, err)
		return
	}
	done, err := s.waitCommand(r.Context(), cmd.ID, 60*time.Second)
	switch {
	case err != nil:
		writeError(w, http.StatusGatewayTimeout, err.Error())
		return
	case done.Status == api.CommandFailed:
		writeError(w, http.StatusBadGateway, done.Error)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(done.Output))
}

type dockerPruneReq struct {
	Trim       bool   `json:"trim"`        // apply the retention policy to worker images
	Dangling   bool   `json:"dangling"`    // untagged images
	Unused     bool   `json:"unused"`      // every image no container uses
	BuildCache bool   `json:"build_cache"` // build cache; CacheDays 0 = all of it
	CacheDays  int    `json:"cache_days"`
	Confirm    string `json:"confirm"` // must be "prune"
}

func (s *Server) handleDockerPrune(w http.ResponseWriter, r *http.Request) {
	d, ok := s.lookupDevice(w, r)
	if !ok {
		return
	}
	var req dockerPruneReq
	if !decode(w, r, &req) {
		return
	}
	if req.Confirm != "prune" {
		writeError(w, http.StatusBadRequest, `type "prune" to confirm`)
		return
	}
	if !req.Trim && !req.Dangling && !req.Unused && !req.BuildCache {
		writeError(w, http.StatusBadRequest, "choose what to remove")
		return
	}
	if req.CacheDays < 0 || req.CacheDays > 365 {
		writeError(w, http.StatusBadRequest, "cache age must be between 0 and 365 days")
		return
	}
	if !hasCap(d, api.CapDockerGC) {
		writeError(w, http.StatusConflict, "this device's agent is too old to prune Docker storage; update it from the Updates page first")
		return
	}
	if d.Offline {
		writeError(w, http.StatusConflict, d.Name+" is offline")
		return
	}
	who := "an admin"
	if id := identityFrom(r); id != nil {
		who = id.Name
	}
	pol := s.dockerGCPolicy(r.Context())
	args := dockerPruneArgs(pol, req.Trim, req.Dangling, req.Unused, req.BuildCache, req.CacheDays)
	args["by"] = who
	cmd, err := s.store.CreateCommand(r.Context(), d.ID, api.ActionDockerPrune, "", args)
	if err != nil {
		s.internal(w, err)
		return
	}
	s.event(r.Context(), d.ID, "warning", "docker.prune.requested", fmt.Sprintf("%s: Docker prune requested on %s (%s)", who, d.Name, pruneWhat(args)))
	writeJSON(w, http.StatusAccepted, cmd)
}

func dockerPruneArgs(pol DockerGCPolicy, trim, dangling, unused, cache bool, cacheDays int) map[string]string {
	a := map[string]string{"keep": strconv.Itoa(pol.Keep), "max_bytes": strconv.FormatInt(pol.maxBytes(), 10), "cache_hours": strconv.Itoa(cacheDays * 24)}
	for k, v := range map[string]bool{"trim": trim, "dangling": dangling, "unused": unused, "build_cache": cache} {
		if v {
			a[k] = "1"
		}
	}
	return a
}

func pruneWhat(a map[string]string) string {
	var out string
	add := func(s string) {
		if out != "" {
			out += ", "
		}
		out += s
	}
	if a["trim"] == "1" {
		add("old worker images")
	}
	if a["unused"] == "1" {
		add("all unused images")
	} else if a["dangling"] == "1" {
		add("untagged images")
	}
	if a["build_cache"] == "1" {
		add("build cache")
	}
	return out
}

// dockerPruneResult records the outcome next to the request in Activity.
func (s *Server) dockerPruneResult(ctx context.Context, d *store.Device, c *api.Command, res api.CommandResult) {
	who := c.Args["by"]
	if who == "" {
		who = "schedule"
	}
	if !res.OK {
		s.event(ctx, d.ID, "error", "docker.prune", fmt.Sprintf("%s: Docker prune on %s failed: %s", who, d.Name, res.Error))
		return
	}
	var out api.DockerPruneResult
	json.Unmarshal([]byte(res.Output), &out)
	s.event(ctx, d.ID, "info", "docker.prune", fmt.Sprintf("%s: Docker prune on %s freed about %s (%s)", who, d.Name, humanSize(out.Reclaimed), pruneWhat(c.Args)))
}

// dockerGCTick runs the scheduled prune on every online device that supports it.
func (s *Server) dockerGCTick(ctx context.Context, now time.Time) {
	pol := s.dockerGCPolicy(ctx)
	if pol.Mode == "off" {
		return
	}
	var baseline time.Time
	if v, err := s.store.GetSetting(ctx, settingDockerGCLastRun); err == nil {
		baseline, _ = time.Parse(time.RFC3339, v)
	}
	if baseline.IsZero() {
		s.store.SetSetting(ctx, settingDockerGCLastRun, now.UTC().Format(time.RFC3339))
		return
	}
	if !pol.schedule().due(baseline, now) {
		return
	}
	s.store.SetSetting(ctx, settingDockerGCLastRun, now.UTC().Format(time.RFC3339))
	devices, err := s.store.ListDevices(ctx)
	if err != nil {
		return
	}
	args := dockerPruneArgs(pol, true, pol.Dangling, false, pol.BuildCache, pol.CacheDays)
	n := 0
	for _, d := range devices {
		if d.Offline || !hasCap(d, api.CapDockerGC) || s.view(d).Status != "online" {
			continue
		}
		if _, err := s.store.CreateCommand(ctx, d.ID, api.ActionDockerPrune, "", args); err == nil {
			n++
		}
	}
	s.event(ctx, "", "info", "docker.prune.scheduled", fmt.Sprintf("Scheduled Docker prune started on %d device(s)", n))
}
