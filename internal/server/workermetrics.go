package server

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Maffewem/Orchestrator/internal/api"
	"github.com/Maffewem/Orchestrator/internal/store"
)

// Per-worker metrics: what each worker collects, the samples agents send, and the history the
// Metrics tab reads.

// Metric IDs a worker can switch on or off. Container metrics come from the agent (Docker stats
// and the device's Caddy proxy); Cloudflare ones from the GraphQL analytics API.
const (
	mCPU            = "cpu"
	mMemory         = "memory"
	mNetwork        = "network"
	mDiskIO         = "disk_io"
	mProcesses      = "processes"
	mThrottling     = "throttling"
	mRestarts       = "restarts"
	mVolumes        = "volumes"
	mProxyRequests  = "proxy_requests"
	mProxyLatency   = "proxy_latency"
	mProxyBandwidth = "proxy_bandwidth"

	mCFRequests  = "cf_requests"
	mCFCPUTime   = "cf_cpu_time"
	mCFWallTime  = "cf_wall_time"
	mCFByStatus  = "cf_by_status"
	mCFByVersion = "cf_by_version"
)

var (
	containerMetricIDs = []string{mCPU, mMemory, mNetwork, mDiskIO, mProcesses, mThrottling, mRestarts, mVolumes, mProxyRequests, mProxyLatency, mProxyBandwidth}
	workerMetricIDs    = []string{mCFRequests, mCFCPUTime, mCFWallTime, mCFByStatus, mCFByVersion}
	metricIntervals    = []int{15, 60, 300}
	metricRetentions   = []int{7, 30, 90}
)

const (
	settingMetricPrefs = "metricprefs:"
	settingVolumeSizes = "volsizes:"
	maxRetentionDays   = 90
)

// metricPrefs is what one worker collects and keeps.
type metricPrefs struct {
	Metrics       []string `json:"metrics"`
	Interval      int      `json:"interval"`       // seconds between stored samples
	RetentionDays int      `json:"retention_days"` // how long samples are kept
}

func defaultMetricPrefs(cloudflare bool) metricPrefs {
	ids := containerMetricIDs
	if cloudflare {
		ids = workerMetricIDs
	}
	return metricPrefs{Metrics: slices.Clone(ids), Interval: 60, RetentionDays: 30}
}

func (p metricPrefs) on(id string) bool { return slices.Contains(p.Metrics, id) }

// metricPrefsKey names a worker's preferences: its project, or for a container that belongs to
// none, the device and container name.
func metricPrefsKey(projectID, deviceID, container string) string {
	if projectID != "" {
		return projectID
	}
	return "w:" + deviceID + "|" + container
}

func (s *Server) metricPrefs(ctx context.Context, key string, cloudflare bool) metricPrefs {
	p := defaultMetricPrefs(cloudflare)
	if v, _ := s.store.GetSetting(ctx, settingMetricPrefs+key); v != "" {
		var saved metricPrefs
		if json.Unmarshal([]byte(v), &saved) == nil {
			p = saved
		}
	}
	if !slices.Contains(metricIntervals, p.Interval) {
		p.Interval = 60
	}
	if !slices.Contains(metricRetentions, p.RetentionDays) {
		p.RetentionDays = 30
	}
	if p.Metrics == nil {
		p.Metrics = []string{}
	}
	return p
}

// cleanMetricPrefs validates preferences sent by the dashboard.
func cleanMetricPrefs(p metricPrefs, cloudflare bool) (metricPrefs, error) {
	allowed := containerMetricIDs
	if cloudflare {
		allowed = workerMetricIDs
	}
	out := metricPrefs{Metrics: []string{}, Interval: p.Interval, RetentionDays: p.RetentionDays}
	for _, id := range p.Metrics {
		if !slices.Contains(allowed, id) {
			return out, fmt.Errorf("unknown metric %q", id)
		}
		if !slices.Contains(out.Metrics, id) {
			out.Metrics = append(out.Metrics, id)
		}
	}
	if !slices.Contains(metricIntervals, out.Interval) {
		return out, fmt.Errorf("interval must be 15, 60 or 300 seconds")
	}
	if !slices.Contains(metricRetentions, out.RetentionDays) {
		return out, fmt.Errorf("retention must be 7, 30 or 90 days")
	}
	return out, nil
}

// --- ingesting agent samples ---

// workerStats is the in-memory state behind stored samples: raw counters at the last stored
// sample (to turn them into rates), CPU and memory seen since then, and the newest reading.
type workerStats struct {
	mu      sync.Mutex
	workers map[string]*workerAcc // device|container
	proxies map[string]*proxyAcc  // device|host
	volumes map[string]volumeSizes
}

type workerAcc struct {
	base     api.ContainerStats
	baseAt   int64
	stored   bool
	cpuSum   float64
	cpuN     int
	memMax   uint64
	latest   api.ContainerStats
	latestAt int64
	pruned   time.Time
	// restarts are the report times of recent Docker restarts; loopEventAt is when the last
	// "restart loop" event was written, so a loop logs one entry rather than one per restart.
	restarts    []int64
	loopEventAt int64
}

// A container Docker restarted this many times within restartLoopWindow is in a restart loop.
const (
	restartLoopCount  = 3
	restartLoopWindow = 15 * 60 // seconds
)

// markUnstable flags containers in a restart loop and returns their names. It works on a copy,
// so the stored device is untouched. Containers Docker reports as restarting or dead count too.
func (s *Server) markUnstable(deviceID string, cs []api.Container) ([]api.Container, []string) {
	if len(cs) == 0 {
		return cs, nil
	}
	out := append([]api.Container(nil), cs...)
	var names []string
	now := time.Now().Unix()
	if s.wstats != nil {
		s.wstats.mu.Lock()
		defer s.wstats.mu.Unlock()
	}
	for i, c := range out {
		looping := c.State == "restarting"
		if s.wstats != nil {
			if acc := s.wstats.workers[deviceID+"|"+c.Name]; acc != nil {
				n := 0
				for _, t := range acc.restarts {
					if now-t <= restartLoopWindow {
						n++
					}
				}
				looping = looping || n >= restartLoopCount
			}
		}
		if looping {
			out[i].Unstable = true
			names = append(names, c.Name)
		}
	}
	return out, names
}

type proxyAcc struct {
	base   api.ProxyHostStats
	baseAt int64
	pruned time.Time
}

type volumeSizes struct {
	At    time.Time
	Sizes map[string]int64
}

func newWorkerStats() *workerStats {
	return &workerStats{workers: map[string]*workerAcc{}, proxies: map[string]*proxyAcc{}, volumes: map[string]volumeSizes{}}
}

// containerOwner finds the project a container belongs to, the way the Workers list does.
func containerOwner(projects []*store.Project, d *store.Device, c api.Container) *store.Project {
	for _, p := range projects {
		if id := c.Labels["orchestrator.project"]; id != "" {
			if p.ID == id {
				return p
			}
			continue
		}
		if p.DeviceID != d.ID {
			continue
		}
		if p.Source.Type == store.SourceCompose && c.ComposeProject != "" && c.ComposeProject == p.Name {
			return p
		}
		if p.Runtime.Adopted && serviceOf(p) == c.Name {
			return p
		}
	}
	return nil
}

func counterRate(cur, base uint64, dt float64) float64 {
	if dt <= 0 {
		return 0
	}
	if cur < base { // the container restarted and its counters began again
		return float64(cur) / dt
	}
	return float64(cur-base) / dt
}

func f64(v float64) *float64 { return &v }
func i64(v int64) *int64     { return &v }

// ingestStats stores a device's new sample for each worker at that worker's interval.
func (s *Server) ingestStats(ctx context.Context, d *store.Device, containers []api.Container, rep *api.StatsReport) {
	if rep == nil {
		return
	}
	projects, err := s.store.ListProjects(ctx)
	if err != nil {
		return
	}
	byID := map[string]api.Container{}
	for _, c := range containers {
		byID[c.ID] = c
	}
	now := time.Now()
	if len(rep.Volumes) > 0 {
		vs := volumeSizes{At: now, Sizes: rep.Volumes}
		s.wstats.mu.Lock()
		s.wstats.volumes[d.ID] = vs
		s.wstats.mu.Unlock()
		// Volumes are measured every 30 minutes; keep the last measurement across server restarts.
		if b, err := json.Marshal(vs); err == nil {
			s.store.SetSetting(ctx, settingVolumeSizes+d.ID, string(b))
		}
	}
	type job struct {
		key    string
		name   string
		sample *store.WorkerSample
		prune  int64
	}
	var jobs []job
	prefsCache := map[string]metricPrefs{}
	prefsFor := func(key string) metricPrefs {
		if p, ok := prefsCache[key]; ok {
			return p
		}
		p := s.metricPrefs(ctx, key, false)
		prefsCache[key] = p
		return p
	}
	s.wstats.mu.Lock()
	for _, cs := range rep.Containers {
		c, ok := byID[cs.ID]
		if !ok {
			c = api.Container{ID: cs.ID, Name: cs.Name}
		}
		owner := ""
		if p := containerOwner(projects, d, c); p != nil {
			owner = p.ID
		}
		key := d.ID + "|" + cs.Name
		acc := s.wstats.workers[key]
		if acc == nil {
			acc = &workerAcc{}
			s.wstats.workers[key] = acc
		}
		prev, seen := acc.latest, acc.latestAt > 0
		acc.latest, acc.latestAt = cs, rep.Time
		if seen && cs.Restarts > prev.Restarts {
			acc.restarts = append(acc.restarts, rep.Time)
			kept := acc.restarts[:0]
			for _, t := range acc.restarts {
				if rep.Time-t <= restartLoopWindow {
					kept = append(kept, t)
				}
			}
			acc.restarts = kept
			if len(acc.restarts) >= restartLoopCount {
				// One entry per loop (renewed every window), not one per restart.
				if rep.Time-acc.loopEventAt > restartLoopWindow {
					acc.loopEventAt = rep.Time
					s.wstatsEvent(d, "warning", "container.restart_loop", fmt.Sprintf("%s on %s is restarting repeatedly (restart %d); further restarts are not logged individually", cs.Name, d.Name, cs.Restarts))
				}
			} else if cs.OOMKilled {
				s.wstatsEvent(d, "warning", "container.oom", fmt.Sprintf("%s on %s restarted after running out of memory", cs.Name, d.Name))
			} else {
				s.wstatsEvent(d, "warning", "container.restarted", fmt.Sprintf("%s on %s restarted by Docker (restart %d)", cs.Name, d.Name, cs.Restarts))
			}
		}
		if seen && cs.Health == "unhealthy" && prev.Health != "unhealthy" {
			s.wstatsEvent(d, "warning", "container.unhealthy", fmt.Sprintf("%s on %s is failing its health check", cs.Name, d.Name))
		}
		acc.cpuSum += cs.CPUPercent
		acc.cpuN++
		acc.memMax = max(acc.memMax, cs.MemUsed)
		if !acc.stored {
			acc.base, acc.baseAt, acc.stored = cs, rep.Time, true
			acc.cpuSum, acc.cpuN, acc.memMax = 0, 0, 0
			continue
		}
		pf := prefsFor(metricPrefsKey(owner, d.ID, cs.Name))
		if rep.Time-acc.baseAt < int64(pf.Interval) {
			continue
		}
		dt := float64(rep.Time - acc.baseAt)
		w := &store.WorkerSample{Time: rep.Time}
		if pf.on(mCPU) && acc.cpuN > 0 {
			w.CPU = f64(math.Round(acc.cpuSum/float64(acc.cpuN)*10) / 10)
		}
		if pf.on(mMemory) {
			w.Mem = i64(int64(acc.memMax))
			if cs.MemLimit > 0 {
				w.MemLimit = i64(int64(cs.MemLimit))
			}
		}
		if pf.on(mNetwork) && !cs.HostNetwork {
			w.NetRx, w.NetTx = f64(counterRate(cs.NetRx, acc.base.NetRx, dt)), f64(counterRate(cs.NetTx, acc.base.NetTx, dt))
		}
		if pf.on(mDiskIO) {
			w.BlkRead, w.BlkWrite = f64(counterRate(cs.BlkRead, acc.base.BlkRead, dt)), f64(counterRate(cs.BlkWrite, acc.base.BlkWrite, dt))
		}
		if pf.on(mProcesses) {
			w.PIDs = i64(int64(cs.PIDs))
		}
		if pf.on(mThrottling) {
			w.Throttled = f64(math.Min(100, counterRate(cs.ThrottledNs, acc.base.ThrottledNs, dt)/1e9*100))
		}
		if pf.on(mRestarts) {
			w.Restarts = i64(int64(cs.Restarts))
			if cs.Health != "" {
				h := cs.Health
				w.Health = &h
			}
		}
		acc.base, acc.baseAt = cs, rep.Time
		acc.cpuSum, acc.cpuN, acc.memMax = 0, 0, 0
		j := job{key: key, name: cs.Name, prune: -1}
		if w.CPU != nil || w.Mem != nil || w.NetRx != nil || w.BlkRead != nil || w.PIDs != nil || w.Throttled != nil || w.Restarts != nil {
			j.sample = w
		}
		if now.Sub(acc.pruned) > time.Hour {
			acc.pruned = now
			j.prune = now.Add(-time.Duration(pf.RetentionDays) * 24 * time.Hour).Unix()
		}
		jobs = append(jobs, j)
	}
	s.wstats.mu.Unlock()
	for _, j := range jobs {
		if j.sample != nil {
			if err := s.store.AddWorkerSample(ctx, d.ID, j.name, *j.sample); err != nil {
				s.log.Warn("store worker sample", "device", d.ID, "container", j.name, "err", err)
			}
		}
		if j.prune > 0 {
			s.store.PruneWorker(ctx, d.ID, j.name, j.prune)
		}
	}
	s.ingestProxy(ctx, d, projects, rep, prefsFor)
}

// wstatsEvent records an activity event without holding up the caller on the database lock.
func (s *Server) wstatsEvent(d *store.Device, level, kind, msg string) {
	go s.event(context.Background(), d.ID, level, kind, msg)
}

// ingestProxy stores each site's requests since its last stored interval.
func (s *Server) ingestProxy(ctx context.Context, d *store.Device, projects []*store.Project, rep *api.StatsReport, prefsFor func(string) metricPrefs) {
	if len(rep.Proxy) == 0 {
		return
	}
	owners := s.proxyHostOwners(ctx, d, projects)
	now := time.Now()
	type job struct {
		host   string
		sample *store.ProxySample
		prune  int64
	}
	var jobs []job
	s.wstats.mu.Lock()
	for _, h := range rep.Proxy {
		host := strings.ToLower(h.Host)
		owner, ok := owners[host]
		if !ok {
			continue // a preview, or a site that no longer exists
		}
		key := d.ID + "|" + host
		acc := s.wstats.proxies[key]
		if acc == nil {
			s.wstats.proxies[key] = &proxyAcc{base: h, baseAt: rep.Time}
			continue
		}
		pf := prefsFor(metricPrefsKey(owner, d.ID, ""))
		if rep.Time-acc.baseAt < int64(pf.Interval) {
			continue
		}
		if !pf.on(mProxyRequests) && !pf.on(mProxyLatency) && !pf.on(mProxyBandwidth) {
			acc.base, acc.baseAt = h, rep.Time
			continue
		}
		p := proxyDelta(acc.base, h)
		p.Time = rep.Time
		if !pf.on(mProxyLatency) {
			p.Hist = nil
		}
		if !pf.on(mProxyBandwidth) {
			p.Bytes = nil
		}
		acc.base, acc.baseAt = h, rep.Time
		j := job{host: host, sample: &p, prune: -1}
		if now.Sub(acc.pruned) > time.Hour {
			acc.pruned = now
			j.prune = now.Add(-time.Duration(pf.RetentionDays) * 24 * time.Hour).Unix()
		}
		jobs = append(jobs, j)
	}
	s.wstats.mu.Unlock()
	for _, j := range jobs {
		if err := s.store.AddProxySample(ctx, d.ID, j.host, *j.sample); err != nil {
			s.log.Warn("store proxy sample", "device", d.ID, "host", j.host, "err", err)
		}
		if j.prune > 0 {
			s.store.PruneProxy(ctx, d.ID, j.host, j.prune)
		}
	}
}

// proxyDelta is the traffic between two cumulative readings. A reading lower than the one
// before means Caddy restarted (every proxy sync restarts it), so the new reading is the delta.
func proxyDelta(base, cur api.ProxyHostStats) store.ProxySample {
	reset := false
	for k, v := range cur.Codes {
		if v < base.Codes[k] {
			reset = true
		}
	}
	if cur.Bytes < base.Bytes {
		reset = true
	}
	if reset {
		base = api.ProxyHostStats{Codes: map[string]float64{}}
	}
	d := func(k string) float64 { return math.Max(0, cur.Codes[k]-base.Codes[k]) }
	p := store.ProxySample{C2xx: d("2xx"), C3xx: d("3xx"), C4xx: d("4xx"), C5xx: d("5xx")}
	p.Requests = p.C2xx + p.C3xx + p.C4xx + p.C5xx + d("1xx")
	p.Bytes = f64(math.Max(0, cur.Bytes-base.Bytes))
	prevCum := map[float64]float64{}
	for _, b := range base.Buckets {
		prevCum[b.Le] = b.Count
	}
	p.Hist = [][2]float64{}
	var last float64
	for _, b := range cur.Buckets {
		cum := math.Max(0, b.Count-prevCum[b.Le])
		p.Hist = append(p.Hist, [2]float64{b.Le, math.Max(0, cum-last)})
		last = math.Max(last, cum)
	}
	return p
}

// proxyHostOwners maps each production hostname the device's proxy serves to its project.
func (s *Server) proxyHostOwners(ctx context.Context, d *store.Device, projects []*store.Project) map[string]string {
	base := s.baseDomain(ctx)
	out := map[string]string{}
	for _, p := range projects {
		if p.DeviceID != d.ID {
			continue
		}
		for _, h := range s.projectProxyHosts(ctx, p, base) {
			out[h] = p.ID
		}
	}
	return out
}

// projectProxyHosts is the production hostnames the proxy serves for a project.
func (s *Server) projectProxyHosts(ctx context.Context, p *store.Project, base string) []string {
	var hosts []string
	if base != "" && s.urlPrefs(ctx, p.ID).Production {
		hosts = append(hosts, slug(p.Name)+"."+base)
	}
	doms, _ := s.store.ListDomains(ctx, p.ID)
	for _, dm := range doms {
		h := strings.ToLower(dm.Hostname)
		if !slices.Contains(hosts, h) {
			hosts = append(hosts, h)
		}
	}
	return hosts
}

// --- reading history ---

// metricsRange turns ?range= into a start time and a bucket size that gives about 90 points.
func metricsRange(r *http.Request) (name string, since, step int64) {
	now := time.Now().Unix()
	switch r.URL.Query().Get("range") {
	case "1h":
		return "1h", now - 3600, 30
	case "7d":
		return "7d", now - 7*86400, 7200
	case "30d":
		return "30d", now - 30*86400, 28800
	default:
		return "24h", now - 86400, 900
	}
}

type workerCurrent struct {
	At          int64   `json:"at"`
	CPU         float64 `json:"cpu"`
	Mem         uint64  `json:"mem"`
	MemLimit    uint64  `json:"mem_limit"`
	PIDs        uint64  `json:"pids"`
	Restarts    int     `json:"restarts"`
	Health      string  `json:"health,omitempty"`
	StartedAt   int64   `json:"started_at"`
	HostNetwork bool    `json:"host_network,omitempty"`
	OOMKilled   bool    `json:"oom_killed,omitempty"`
}

type volumeView struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
}

type proxyPoint struct {
	Time     int64    `json:"t"`
	Requests float64  `json:"requests"`
	C2xx     float64  `json:"c2xx"`
	C3xx     float64  `json:"c3xx"`
	C4xx     float64  `json:"c4xx"`
	C5xx     float64  `json:"c5xx"`
	Bytes    *float64 `json:"bytes,omitempty"`
	P50      *float64 `json:"p50,omitempty"` // milliseconds
	P95      *float64 `json:"p95,omitempty"`
}

type proxyView struct {
	Hosts  []string     `json:"hosts"`
	Series []proxyPoint `json:"series"`
	Totals proxyPoint   `json:"totals"`
	P99    *float64     `json:"p99,omitempty"`
}

type workerMetricsView struct {
	Supported  bool                 `json:"supported"`
	ProjectID  string               `json:"project_id,omitempty"`
	Prefs      metricPrefs          `json:"prefs"`
	Range      string               `json:"range"`
	Step       int64                `json:"step"`
	Current    *workerCurrent       `json:"current,omitempty"`
	Series     []store.WorkerSample `json:"series"`
	Restarts   int64                `json:"restarts"`
	Volumes    []volumeView         `json:"volumes"`
	VolumesAt  *time.Time           `json:"volumes_at,omitempty"`
	Proxy      *proxyView           `json:"proxy,omitempty"`
	ProxyNotes string               `json:"proxy_note,omitempty"`
}

// histQuantile estimates a percentile in milliseconds from bucketed counts.
func histQuantile(hist map[float64]float64, q float64) *float64 {
	les := make([]float64, 0, len(hist))
	var total float64
	for le, n := range hist {
		les = append(les, le)
		total += n
	}
	if total <= 0 {
		return nil
	}
	sort.Float64s(les)
	target, cum, lo := q*total, 0.0, 0.0
	for _, le := range les {
		n := hist[le]
		if cum+n >= target && n > 0 {
			v := (lo + (le-lo)*((target-cum)/n)) * 1000
			v = math.Round(v*10) / 10
			return &v
		}
		cum += n
		lo = le
	}
	v := lo * 1000
	return &v
}

// proxySeries buckets stored proxy samples and totals them.
func proxySeries(samples []store.ProxySample, step int64) ([]proxyPoint, proxyPoint, *float64) {
	type agg struct {
		p    proxyPoint
		hist map[float64]float64
	}
	buckets := map[int64]*agg{}
	all := map[float64]float64{}
	var tot proxyPoint
	for _, s := range samples {
		b := (s.Time / step) * step
		a := buckets[b]
		if a == nil {
			a = &agg{p: proxyPoint{Time: b}, hist: map[float64]float64{}}
			buckets[b] = a
		}
		a.p.Requests += s.Requests
		a.p.C2xx += s.C2xx
		a.p.C3xx += s.C3xx
		a.p.C4xx += s.C4xx
		a.p.C5xx += s.C5xx
		if s.Bytes != nil {
			if a.p.Bytes == nil {
				a.p.Bytes = f64(0)
			}
			*a.p.Bytes += *s.Bytes
		}
		for _, h := range s.Hist {
			a.hist[h[0]] += h[1]
			all[h[0]] += h[1]
		}
		tot.Requests += s.Requests
		tot.C2xx += s.C2xx
		tot.C3xx += s.C3xx
		tot.C4xx += s.C4xx
		tot.C5xx += s.C5xx
		if s.Bytes != nil {
			if tot.Bytes == nil {
				tot.Bytes = f64(0)
			}
			*tot.Bytes += *s.Bytes
		}
	}
	keys := make([]int64, 0, len(buckets))
	for k := range buckets {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	out := make([]proxyPoint, 0, len(keys))
	for _, k := range keys {
		a := buckets[k]
		a.p.P50, a.p.P95 = histQuantile(a.hist, 0.5), histQuantile(a.hist, 0.95)
		out = append(out, a.p)
	}
	tot.P50, tot.P95 = histQuantile(all, 0.5), histQuantile(all, 0.95)
	return out, tot, histQuantile(all, 0.99)
}

func (s *Server) handleWorkerMetrics(w http.ResponseWriter, r *http.Request) {
	d, ok := s.lookupDevice(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	name := r.PathValue("name")
	var c *api.Container
	for i := range d.Containers {
		if d.Containers[i].Name == name {
			c = &d.Containers[i]
		}
	}
	projects, err := s.store.ListProjects(ctx)
	if err != nil {
		s.internal(w, err)
		return
	}
	var owner *store.Project
	if c != nil {
		owner = containerOwner(projects, d, *c)
	}
	ownerID := ""
	if owner != nil {
		ownerID = owner.ID
	}
	rng, since, step := metricsRange(r)
	v := workerMetricsView{Supported: hasCap(d, api.CapStats), ProjectID: ownerID, Range: rng, Step: step,
		Prefs: s.metricPrefs(ctx, metricPrefsKey(ownerID, d.ID, name), false), Volumes: []volumeView{}}
	if v.Series, err = s.store.WorkerSeries(ctx, d.ID, name, since, step); err != nil {
		s.internal(w, err)
		return
	}
	v.Restarts, _ = s.store.WorkerRestartsSince(ctx, d.ID, name, since)
	s.wstats.mu.Lock()
	_, haveVols := s.wstats.volumes[d.ID]
	s.wstats.mu.Unlock()
	if !haveVols {
		var vs volumeSizes
		if v, _ := s.store.GetSetting(ctx, settingVolumeSizes+d.ID); v != "" && json.Unmarshal([]byte(v), &vs) == nil {
			s.wstats.mu.Lock()
			if _, ok := s.wstats.volumes[d.ID]; !ok {
				s.wstats.volumes[d.ID] = vs
			}
			s.wstats.mu.Unlock()
		}
	}
	s.wstats.mu.Lock()
	if acc := s.wstats.workers[d.ID+"|"+name]; acc != nil && acc.latestAt > 0 {
		l := acc.latest
		v.Current = &workerCurrent{At: acc.latestAt, CPU: l.CPUPercent, Mem: l.MemUsed, MemLimit: l.MemLimit, PIDs: l.PIDs,
			Restarts: l.Restarts, Health: l.Health, StartedAt: l.StartedAt, HostNetwork: l.HostNetwork, OOMKilled: l.OOMKilled}
	}
	if vs, ok := s.wstats.volumes[d.ID]; ok && c != nil && v.Prefs.on(mVolumes) {
		at := vs.At
		v.VolumesAt = &at
		for _, name := range c.Volumes {
			if size, ok := vs.Sizes[name]; ok {
				v.Volumes = append(v.Volumes, volumeView{Name: name, Size: size})
			}
		}
	}
	s.wstats.mu.Unlock()
	if owner != nil && (v.Prefs.on(mProxyRequests) || v.Prefs.on(mProxyLatency) || v.Prefs.on(mProxyBandwidth)) && c != nil && c.Labels["orchestrator.preview"] == "" {
		hosts := s.projectProxyHosts(ctx, owner, s.baseDomain(ctx))
		if len(hosts) == 0 {
			v.ProxyNotes = "This worker has no domain on the device's proxy, so there is no proxy traffic to count."
		} else {
			samples, err := s.store.ProxySamples(ctx, d.ID, hosts, since)
			if err != nil {
				s.internal(w, err)
				return
			}
			pv := &proxyView{Hosts: hosts}
			pv.Series, pv.Totals, pv.P99 = proxySeries(samples, step)
			v.Proxy = pv
		}
	}
	writeJSON(w, http.StatusOK, v)
}

// --- preferences endpoints ---

type metricPrefsView struct {
	Key        string      `json:"key"`
	Cloudflare bool        `json:"cloudflare"`
	Prefs      metricPrefs `json:"prefs"`
	// Storage estimates the SQLite space this worker's history takes at these settings, in bytes.
	Storage int64 `json:"storage"`
}

// estimateStorage is a rough size of a worker's history: about 100 bytes per stored row,
// one row per interval for the container and one per proxied site; Cloudflare stores hourly.
func estimateStorage(p metricPrefs, cloudflare bool, sites int) int64 {
	if len(p.Metrics) == 0 {
		return 0
	}
	days := int64(p.RetentionDays)
	if cloudflare {
		rows := days * 24 * 8 // one summary row plus about seven breakdown rows an hour
		return rows * 80
	}
	rows := days * 86400 / int64(p.Interval)
	perRow := int64(60 + 8*len(p.Metrics))
	proxyRows := int64(0)
	if p.on(mProxyRequests) || p.on(mProxyLatency) || p.on(mProxyBandwidth) {
		proxyRows = rows * int64(max(sites, 1))
	}
	return rows*perRow + proxyRows*120
}

func (s *Server) handleGetProjectMetricPrefs(w http.ResponseWriter, r *http.Request) {
	p, ok := s.lookupProject(w, r)
	if !ok {
		return
	}
	cf := p.Source.Type == store.SourceCloudflare
	pf := s.metricPrefs(r.Context(), p.ID, cf)
	writeJSON(w, http.StatusOK, metricPrefsView{Key: p.ID, Cloudflare: cf, Prefs: pf,
		Storage: estimateStorage(pf, cf, len(s.projectProxyHosts(r.Context(), p, s.baseDomain(r.Context()))))})
}

func (s *Server) handleSetProjectMetricPrefs(w http.ResponseWriter, r *http.Request) {
	p, ok := s.lookupProject(w, r)
	if !ok {
		return
	}
	s.saveMetricPrefs(w, r, p.ID, p.Source.Type == store.SourceCloudflare, len(s.projectProxyHosts(r.Context(), p, s.baseDomain(r.Context()))))
}

// workerPrefsKey resolves a device container to the key its preferences live under.
func (s *Server) workerPrefsKey(ctx context.Context, d *store.Device, name string) (string, error) {
	projects, err := s.store.ListProjects(ctx)
	if err != nil {
		return "", err
	}
	for _, c := range d.Containers {
		if c.Name == name {
			if p := containerOwner(projects, d, c); p != nil {
				return p.ID, nil
			}
		}
	}
	return metricPrefsKey("", d.ID, name), nil
}

func (s *Server) handleGetWorkerMetricPrefs(w http.ResponseWriter, r *http.Request) {
	d, ok := s.lookupDevice(w, r)
	if !ok {
		return
	}
	key, err := s.workerPrefsKey(r.Context(), d, r.PathValue("name"))
	if err != nil {
		s.internal(w, err)
		return
	}
	pf := s.metricPrefs(r.Context(), key, false)
	writeJSON(w, http.StatusOK, metricPrefsView{Key: key, Prefs: pf, Storage: estimateStorage(pf, false, 1)})
}

func (s *Server) handleSetWorkerMetricPrefs(w http.ResponseWriter, r *http.Request) {
	d, ok := s.lookupDevice(w, r)
	if !ok {
		return
	}
	key, err := s.workerPrefsKey(r.Context(), d, r.PathValue("name"))
	if err != nil {
		s.internal(w, err)
		return
	}
	s.saveMetricPrefs(w, r, key, false, 1)
}

func (s *Server) saveMetricPrefs(w http.ResponseWriter, r *http.Request, key string, cloudflare bool, sites int) {
	var in metricPrefs
	if !decode(w, r, &in) {
		return
	}
	pf, err := cleanMetricPrefs(in, cloudflare)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	b, _ := json.Marshal(pf)
	if err := s.store.SetSetting(r.Context(), settingMetricPrefs+key, string(b)); err != nil {
		s.internal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, metricPrefsView{Key: key, Cloudflare: cloudflare, Prefs: pf, Storage: estimateStorage(pf, cloudflare, sites)})
}

// metricsPruneLoop removes history past the longest retention once an hour, for workers that
// no longer report (their own pruning only runs while samples arrive).
func (s *Server) metricsPruneLoop(ctx context.Context) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		if err := s.store.PruneMetricsBefore(ctx, time.Now().Add(-(maxRetentionDays+1)*24*time.Hour)); err != nil && ctx.Err() == nil {
			s.log.Warn("prune metrics", "err", err)
		}
		if err := s.pruneDeviceHistory(ctx); err != nil && ctx.Err() == nil {
			s.log.Warn("prune device history", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// oomKilled names the containers whose last stop was the kernel's out-of-memory killer and that
// are not healthily running now (stopped, or restart-looping).
func (s *Server) oomKilled(deviceID string, cs []api.Container) map[string]bool {
	out := map[string]bool{}
	if s.wstats == nil {
		return out
	}
	s.wstats.mu.Lock()
	defer s.wstats.mu.Unlock()
	for _, c := range cs {
		if acc := s.wstats.workers[deviceID+"|"+c.Name]; acc != nil && acc.latest.OOMKilled && (c.State != "running" || c.Unstable) {
			out[c.Name] = true
		}
	}
	return out
}
