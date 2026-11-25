package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Maffewem/Orchestrator/internal/store"
)

// Failover history and the capacity check for the cluster (Beta).
//
// History: every failover and fail back is recorded as one entry (what failed, where the worker
// went, how long the move took and how it ended). Entries are kept in one setting, newest first.
//
// Capacity: a worker's footprint is estimated from what it uses now (memory and CPU from the
// newest sample, disk from its volumes or newest backup) and compared with what the candidate
// devices have free. That drives a warning before a target is chosen and a per-device
// "can the cluster survive losing this device" answer.

const (
	clusterHistoryKey = "cluster:history"
	maxHistory        = 200
	// historyStale is how long a move may stay "running" with no migration to match before it is
	// closed as unknown (for example after a server restart).
	historyStale = time.Hour

	defaultFailBackDelay = 300
	minFailBackDelay     = 60

	kindFailover = "failover"
	kindFailback = "failback"

	histRunning = "running"
	histDone    = "done"
	histFailed  = "failed"
)

// failoverEntry is one automatic move of a worker.
type failoverEntry struct {
	ID        string     `json:"id"`
	ProjectID string     `json:"project_id"`
	Worker    string     `json:"worker"`
	Kind      string     `json:"kind"` // failover | failback
	At        time.Time  `json:"at"`
	Finished  *time.Time `json:"finished_at,omitempty"`
	FromID    string     `json:"from_id"`
	FromName  string     `json:"from_name"`
	ToID      string     `json:"to_id,omitempty"`
	ToName    string     `json:"to_name,omitempty"`
	Status    string     `json:"status"` // running | done | failed
	// Reason says what triggered it, for example "pi-old offline for 2m10s".
	Reason string `json:"reason,omitempty"`
	Error  string `json:"error,omitempty"`
	// OfflineSeconds is how long the source device had been silent when the failover started.
	OfflineSeconds int  `json:"offline_seconds,omitempty"`
	WithData       bool `json:"with_data"`
	// DurationSeconds is how long the move itself took, once it has finished.
	DurationSeconds int `json:"duration_seconds,omitempty"`
	// Warnings are capacity concerns noted when the target was chosen.
	Warnings []string `json:"warnings,omitempty"`
}

var histMu sync.Mutex

func (s *Server) loadHistory(ctx context.Context) []failoverEntry {
	var out []failoverEntry
	if raw, err := s.store.GetSetting(ctx, clusterHistoryKey); err == nil && raw != "" {
		json.Unmarshal([]byte(raw), &out)
	}
	return out
}

func (s *Server) saveHistory(ctx context.Context, h []failoverEntry) {
	if len(h) > maxHistory {
		h = h[:maxHistory]
	}
	b, _ := json.Marshal(h)
	if err := s.store.SetSetting(ctx, clusterHistoryKey, string(b)); err != nil {
		s.log.Error("save failover history", "err", err)
	}
}

// addHistory records a new entry at the top and returns its id. A failed attempt that repeats an
// identical newest failure (same worker, source and error) is not stored again, so a worker that
// cannot be moved does not fill the list every retry.
func (s *Server) addHistory(ctx context.Context, e failoverEntry) string {
	histMu.Lock()
	defer histMu.Unlock()
	h := s.loadHistory(ctx)
	if e.Status == histFailed {
		for _, old := range h {
			if old.ProjectID != e.ProjectID {
				continue
			}
			if old.Status == histFailed && old.FromID == e.FromID && old.Kind == e.Kind && old.Error == e.Error {
				return old.ID
			}
			break
		}
	}
	e.ID = fmt.Sprintf("%d", time.Now().UnixNano())
	if e.At.IsZero() {
		e.At = time.Now().UTC()
	}
	h = append([]failoverEntry{e}, h...)
	s.saveHistory(ctx, h)
	return e.ID
}

func (s *Server) failHistory(ctx context.Context, id, msg string) {
	s.updateHistory(ctx, id, func(e *failoverEntry) {
		now := time.Now().UTC()
		e.Status, e.Error, e.Finished = histFailed, msg, &now
	})
}

func (s *Server) updateHistory(ctx context.Context, id string, f func(e *failoverEntry)) {
	histMu.Lock()
	defer histMu.Unlock()
	h := s.loadHistory(ctx)
	for i := range h {
		if h[i].ID == id {
			f(&h[i])
			s.saveHistory(ctx, h)
			return
		}
	}
}

// settleHistory closes entries still marked running by looking at the migration that carries
// them, and returns the projects whose fail back just failed (so they are retried later).
func (s *Server) settleHistory(ctx context.Context) {
	histMu.Lock()
	defer histMu.Unlock()
	h := s.loadHistory(ctx)
	changed := false
	for i := range h {
		e := &h[i]
		if e.Status != histRunning {
			continue
		}
		v := s.migrationView(e.ProjectID)
		if v != nil && !v.Started.Before(e.At.Add(-2*time.Second)) && v.Status != migRunning {
			fin := v.Finished
			if fin.IsZero() {
				fin = time.Now().UTC()
			}
			e.Finished = &fin
			e.DurationSeconds = int(fin.Sub(v.Started).Round(time.Second) / time.Second)
			if v.Status == migDone {
				e.Status = histDone
			} else {
				e.Status, e.Error = histFailed, v.Error
			}
			changed = true
			continue
		}
		if time.Since(e.At) > historyStale {
			now := time.Now().UTC()
			e.Status, e.Error, e.Finished = histFailed, "No result was recorded for this move (the server may have restarted).", &now
			changed = true
		}
	}
	if changed {
		s.saveHistory(ctx, h)
	}
}

// --- capacity ---

// workerNeed is the estimated footprint of one worker. A zero field means "unknown".
type workerNeed struct {
	Cores float64 `json:"cores,omitempty"`
	Mem   uint64  `json:"mem,omitempty"`
	Disk  int64   `json:"disk,omitempty"`
}

func (n workerNeed) unknown() bool { return n.Cores == 0 && n.Mem == 0 && n.Disk == 0 }

// headroom is kept free on a target so it is not filled to the brim.
const headroom = 0.9

// estimateNeed reads what the worker uses on its current device.
func (s *Server) estimateNeed(ctx context.Context, p *store.Project, d *store.Device) workerNeed {
	var n workerNeed
	if d == nil {
		return n
	}
	name := serviceOf(p)
	s.wstats.mu.Lock()
	if acc := s.wstats.workers[d.ID+"|"+name]; acc != nil && acc.latestAt > 0 {
		n.Cores = acc.latest.CPUPercent / 100
		n.Mem = acc.latest.MemUsed
		if acc.memMax > n.Mem {
			n.Mem = acc.memMax
		}
	}
	vs, haveVols := s.wstats.volumes[d.ID]
	s.wstats.mu.Unlock()
	if c := findByName(d, name); c != nil && haveVols {
		var total int64
		for _, v := range c.Volumes {
			total += vs.Sizes[v]
		}
		n.Disk = total
	}
	if n.Disk == 0 {
		if bs, err := s.store.ListBackups(ctx, d.ID); err == nil {
			for _, b := range bs {
				if b.ContainerName == name && b.Status == store.BackupComplete {
					n.Disk = b.Size
					break
				}
			}
		}
	}
	return n
}

// freeOn reports what a device has free; a total of zero means the device never reported it.
func freeOn(d *store.Device) (cores float64, mem uint64, disk int64) {
	m := d.Metrics
	if d.Host.CPUs > 0 {
		cores = float64(d.Host.CPUs) * (1 - m.CPUPercent/100)
		if cores < 0 {
			cores = 0
		}
	}
	if m.MemoryTotal > m.MemoryUsed {
		mem = m.MemoryTotal - m.MemoryUsed
	}
	if m.DiskTotal > m.DiskUsed {
		disk = int64(m.DiskTotal - m.DiskUsed)
	}
	return
}

func humanBytes(b uint64) string {
	const u = 1024.0
	f := float64(b)
	for _, s := range []string{"B", "KB", "MB", "GB", "TB"} {
		if f < u || s == "TB" {
			if s == "B" {
				return fmt.Sprintf("%d B", b)
			}
			return fmt.Sprintf("%.1f %s", f, s)
		}
		f /= u
	}
	return ""
}

// fitProblems lists why a worker with the given need would not comfortably fit on d, taking
// reserved (already promised to other workers in a what-if) into account.
func fitProblems(n workerNeed, d *store.Device, reserved workerNeed) []string {
	cores, mem, disk := freeOn(d)
	var out []string
	if n.Mem > 0 && d.Metrics.MemoryTotal > 0 && float64(n.Mem+reserved.Mem) > float64(mem)*headroom {
		out = append(out, fmt.Sprintf("needs about %s of memory, %s free", humanBytes(n.Mem+reserved.Mem), humanBytes(mem)))
	}
	if n.Cores > 0 && d.Host.CPUs > 0 && n.Cores+reserved.Cores > cores*headroom {
		out = append(out, fmt.Sprintf("uses about %.1f cores, %.1f free", n.Cores+reserved.Cores, cores))
	}
	if n.Disk > 0 && d.Metrics.DiskTotal > 0 && float64(n.Disk+reserved.Disk) > float64(disk)*headroom {
		out = append(out, fmt.Sprintf("needs about %s of disk, %s free", humanBytes(uint64(n.Disk+reserved.Disk)), humanBytes(uint64(disk))))
	}
	return out
}

type targetFit struct {
	DeviceID string   `json:"device_id"`
	Device   string   `json:"device"`
	Fits     bool     `json:"fits"`
	Problems []string `json:"problems,omitempty"`
}

type deviceCapacity struct {
	DeviceID string   `json:"device_id"`
	Device   string   `json:"device"`
	Cores    int      `json:"cores"`
	MemFree  uint64   `json:"mem_free"`
	DiskFree int64    `json:"disk_free"`
	Workers  int      `json:"workers"` // workers with failover on whose main copy is here
	Online   bool     `json:"online"`
	CanLose  bool     `json:"can_lose"`
	Stranded []string `json:"stranded,omitempty"` // workers that could not be placed if this device were lost
	Reasons  []string `json:"reasons,omitempty"`
}

type capacityView struct {
	// SurvivesOne is true when losing any single device still leaves room for every worker that has failover on.
	SurvivesOne bool             `json:"survives_one"`
	Devices     []deviceCapacity `json:"devices"`
	// Enabled is false when no worker has failover on, so there is nothing to check.
	Enabled bool `json:"enabled"`
}

// capacityReport simulates losing each device in turn: the failover workers on it are placed one
// by one on the remaining online devices, each placement using up the room it needs.
func (s *Server) capacityReport(ctx context.Context, devices []*store.Device, projects []*store.Project) (capacityView, map[string][]targetFit) {
	v := capacityView{SurvivesOne: true, Devices: []deviceCapacity{}}
	targets := map[string][]targetFit{}
	byID := map[string]*store.Device{}
	for _, d := range devices {
		byID[d.ID] = d
	}
	type fw struct {
		p    *store.Project
		pol  ClusterPolicy
		need workerNeed
	}
	byDev := map[string][]fw{}
	for _, p := range projects {
		if p.Source.Type == store.SourceCloudflare {
			continue
		}
		pol := s.clusterPolicy(ctx, p.ID)
		if !pol.Failover {
			continue
		}
		v.Enabled = true
		need := s.estimateNeed(ctx, p, byID[p.DeviceID])
		byDev[p.DeviceID] = append(byDev[p.DeviceID], fw{p, pol, need})
		for _, d := range s.failoverCandidates(p, pol, devices) {
			f := targetFit{DeviceID: d.ID, Device: d.Name}
			f.Problems = fitProblems(need, d, workerNeed{})
			f.Fits = len(f.Problems) == 0
			targets[p.ID] = append(targets[p.ID], f)
		}
	}
	for _, d := range devices {
		_, mem, disk := freeOn(d)
		dc := deviceCapacity{DeviceID: d.ID, Device: d.Name, Cores: d.Host.CPUs, MemFree: mem, DiskFree: disk, Online: !d.Offline, Workers: len(byDev[d.ID]), CanLose: true}
		reserved := map[string]workerNeed{}
		for _, w := range byDev[d.ID] {
			placed := false
			var why string
			for _, c := range s.failoverCandidates(w.p, w.pol, devices) {
				if c.ID == d.ID {
					continue
				}
				r := reserved[c.ID]
				if probs := fitProblems(w.need, c, r); len(probs) > 0 {
					why = c.Name + " " + strings.Join(probs, ", ")
					continue
				}
				r.Cores += w.need.Cores
				r.Mem += w.need.Mem
				r.Disk += w.need.Disk
				reserved[c.ID] = r
				placed = true
				break
			}
			if !placed {
				dc.CanLose = false
				dc.Stranded = append(dc.Stranded, w.p.Name)
				if why == "" {
					why = "no other online device is allowed and able to run it"
				}
				dc.Reasons = append(dc.Reasons, w.p.Name+": "+why)
			}
		}
		if !dc.CanLose {
			v.SurvivesOne = false
		}
		v.Devices = append(v.Devices, dc)
	}
	sort.Slice(v.Devices, func(i, j int) bool { return v.Devices[i].Device < v.Devices[j].Device })
	return v, targets
}

// --- API ---

func (s *Server) handleClusterHistory(w http.ResponseWriter, r *http.Request) {
	h := s.loadHistory(r.Context())
	if id := r.URL.Query().Get("project"); id != "" {
		var f []failoverEntry
		for _, e := range h {
			if e.ProjectID == id {
				f = append(f, e)
			}
		}
		h = f
	}
	if h == nil {
		h = []failoverEntry{}
	}
	if len(h) > 100 {
		h = h[:100]
	}
	writeJSON(w, http.StatusOK, h)
}
