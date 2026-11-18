package agent

import (
	"bufio"
	"context"
	"io"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Maffewem/Orchestrator/internal/api"
)

const (
	// statsEvery is how often the agent samples its containers. The server stores samples at
	// each worker's chosen interval (15 s or longer), so this is the finest it can choose.
	statsEvery = 15 * time.Second
	// volumesEvery is how often named volumes are re-measured; measuring walks every file.
	volumesEvery = 30 * time.Minute
	// proxyMetricsURL is where the managed Caddy proxy serves its Prometheus metrics. It runs on
	// the host network and the Caddyfile binds this address to loopback only.
	proxyMetricsURL = "http://127.0.0.1:2019/metrics"
)

// statsState is what the sampler keeps between samples.
type statsState struct {
	mu      sync.Mutex
	pending *api.StatsReport
	cpu     map[string]cpuMark // container ID -> previous CPU counter
	volAt   time.Time
}

type cpuMark struct {
	total uint64 // container CPU time, ns
	at    time.Time
}

// statsLoop samples the containers every statsEvery and leaves the newest sample for the next
// heartbeat to carry.
func (a *Agent) statsLoop(ctx context.Context) {
	t := time.NewTicker(statsEvery)
	defer t.Stop()
	for {
		sctx, cancel := context.WithTimeout(ctx, statsEvery)
		rep := a.sampleStats(sctx)
		cancel()
		if rep != nil {
			a.stats.mu.Lock()
			if a.stats.pending != nil && len(rep.Volumes) == 0 {
				rep.Volumes = a.stats.pending.Volumes // keep a measurement the server has not seen yet
			}
			a.stats.pending = rep
			a.stats.mu.Unlock()
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// takeStats hands the pending sample to a heartbeat.
func (a *Agent) takeStats() *api.StatsReport {
	a.stats.mu.Lock()
	defer a.stats.mu.Unlock()
	r := a.stats.pending
	a.stats.pending = nil
	return r
}

// dockerStats is the part of GET /containers/{id}/stats the agent reads.
type dockerStats struct {
	CPU struct {
		Usage struct {
			Total uint64 `json:"total_usage"`
		} `json:"cpu_usage"`
		Throttling struct {
			ThrottledTime uint64 `json:"throttled_time"`
		} `json:"throttling_data"`
	} `json:"cpu_stats"`
	Memory struct {
		Usage uint64            `json:"usage"`
		Limit uint64            `json:"limit"`
		Stats map[string]uint64 `json:"stats"`
	} `json:"memory_stats"`
	Networks map[string]struct {
		Rx uint64 `json:"rx_bytes"`
		Tx uint64 `json:"tx_bytes"`
	} `json:"networks"`
	Blkio struct {
		Bytes []struct {
			Op    string `json:"op"`
			Value uint64 `json:"value"`
		} `json:"io_service_bytes_recursive"`
	} `json:"blkio_stats"`
	PIDs struct {
		Current uint64 `json:"current"`
	} `json:"pids_stats"`
}

// memUsed is the working set the way `docker stats` shows it: usage minus reclaimable page cache.
func (s dockerStats) memUsed() uint64 {
	u := s.Memory.Usage
	for _, k := range []string{"inactive_file", "total_inactive_file"} { // cgroup v2, v1
		if v, ok := s.Memory.Stats[k]; ok && v < u {
			return u - v
		}
	}
	return u
}

func (s dockerStats) blk() (read, write uint64) {
	for _, b := range s.Blkio.Bytes {
		switch strings.ToLower(b.Op) {
		case "read":
			read += b.Value
		case "write":
			write += b.Value
		}
	}
	return
}

type dockerInspectState struct {
	RestartCount int `json:"RestartCount"`
	State        struct {
		StartedAt string `json:"StartedAt"`
		OOMKilled bool   `json:"OOMKilled"`
		Health    *struct {
			Status string `json:"Status"`
		} `json:"Health"`
	} `json:"State"`
	HostConfig struct {
		NetworkMode string `json:"NetworkMode"`
	} `json:"HostConfig"`
}

// sampleStats measures every running container once. It returns nil when Docker is unreachable.
func (a *Agent) sampleStats(ctx context.Context) *api.StatsReport {
	list, err := a.docker.listContainers(ctx, map[string][]string{"status": {"running"}})
	if err != nil {
		return nil
	}
	now := time.Now()
	rep := &api.StatsReport{Time: now.Unix(), Containers: []api.ContainerStats{}}
	a.stats.mu.Lock()
	if a.stats.cpu == nil {
		a.stats.cpu = map[string]cpuMark{}
	}
	prev := a.stats.cpu
	measureVolumes := now.Sub(a.stats.volAt) >= volumesEvery
	a.stats.mu.Unlock()
	next := map[string]cpuMark{}
	devMem := meminfo()["MemTotal"]
	for _, c := range list {
		cs, ok := a.containerStats(ctx, c.ID, now, prev[c.ID], next)
		if !ok {
			continue
		}
		cs.Name = c.toAPI().Name
		if devMem > 0 && cs.MemLimit >= devMem {
			cs.MemLimit = 0 // Docker reports the device's memory when no limit is set
		}
		rep.Containers = append(rep.Containers, cs)
	}
	a.stats.mu.Lock()
	a.stats.cpu = next
	a.stats.mu.Unlock()
	if measureVolumes {
		if v, err := a.volumeSizes(ctx); err == nil {
			rep.Volumes = v
			a.stats.mu.Lock()
			a.stats.volAt = now
			a.stats.mu.Unlock()
		}
	}
	rep.Proxy = a.proxyStats(ctx)
	return rep
}

func (a *Agent) containerStats(ctx context.Context, id string, now time.Time, prev cpuMark, next map[string]cpuMark) (api.ContainerStats, bool) {
	var st dockerStats
	// one-shot skips Docker's own one-second CPU sample (API 1.41+); older engines ignore it.
	if err := a.docker.call(ctx, http.MethodGet, "/containers/"+id+"/stats", url.Values{"stream": {"false"}, "one-shot": {"true"}}, nil, &st); err != nil {
		return api.ContainerStats{}, false
	}
	var in dockerInspectState
	if err := a.docker.call(ctx, http.MethodGet, "/containers/"+id+"/json", nil, nil, &in); err != nil {
		return api.ContainerStats{}, false
	}
	cs := api.ContainerStats{ID: id, MemUsed: st.memUsed(), MemLimit: st.Memory.Limit, PIDs: st.PIDs.Current,
		ThrottledNs: st.CPU.Throttling.ThrottledTime, Restarts: in.RestartCount, OOMKilled: in.State.OOMKilled}
	next[id] = cpuMark{total: st.CPU.Usage.Total, at: now}
	if prev.total > 0 && st.CPU.Usage.Total >= prev.total {
		if dt := now.Sub(prev.at); dt > 0 {
			cs.CPUPercent = math.Round(float64(st.CPU.Usage.Total-prev.total)/float64(dt.Nanoseconds())*1000) / 10
		}
	}
	for _, n := range st.Networks {
		cs.NetRx += n.Rx
		cs.NetTx += n.Tx
	}
	cs.HostNetwork = in.HostConfig.NetworkMode == "host"
	cs.BlkRead, cs.BlkWrite = st.blk()
	if t, err := time.Parse(time.RFC3339Nano, in.State.StartedAt); err == nil && t.Year() > 1 {
		cs.StartedAt = t.Unix()
	}
	if in.State.Health != nil {
		cs.Health = in.State.Health.Status
	}
	return cs, true
}

// volumeSizes asks Docker for the size of every named volume.
func (a *Agent) volumeSizes(ctx context.Context) (map[string]int64, error) {
	var df struct {
		Volumes []struct {
			Name  string `json:"Name"`
			Usage *struct {
				Size int64 `json:"Size"`
			} `json:"UsageData"`
		} `json:"Volumes"`
	}
	if err := a.docker.call(ctx, http.MethodGet, "/system/df", url.Values{"type": {"volume"}}, nil, &df); err != nil {
		return nil, err
	}
	out := map[string]int64{}
	for _, v := range df.Volumes {
		if v.Usage != nil && v.Usage.Size >= 0 {
			out[v.Name] = v.Usage.Size
		}
	}
	return out, nil
}

// proxyStats reads the managed Caddy proxy's metrics. It returns nil when there is no proxy or
// it predates metrics (the Caddyfile is rewritten on the next proxy sync).
func (a *Agent) proxyStats(ctx context.Context) []api.ProxyHostStats {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, proxyMetricsURL, nil)
	if err != nil {
		return nil
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	return parseCaddyMetrics(io.LimitReader(resp.Body, 8<<20))
}

// parseCaddyMetrics folds Caddy's Prometheus text output into per-host counters. It reads
// caddy_http_request_duration_seconds (a histogram labelled by host and code when the Caddyfile
// turns on per-host metrics) and caddy_http_response_size_bytes_sum. Caddy counts a request
// once per instrumented handler, so for each host only the busiest handler (the site's
// top-level route) is kept.
func parseCaddyMetrics(r io.Reader) []api.ProxyHostStats {
	type acc struct {
		codes   map[string]float64
		buckets map[float64]float64
		bytes   float64
		total   float64
	}
	byHost := map[string]map[string]*acc{} // host -> handler -> counters
	get := func(host, handler string) *acc {
		if byHost[host] == nil {
			byHost[host] = map[string]*acc{}
		}
		a := byHost[host][handler]
		if a == nil {
			a = &acc{codes: map[string]float64{}, buckets: map[float64]float64{}}
			byHost[host][handler] = a
		}
		return a
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if line == "" || line[0] == '#' {
			continue
		}
		name, labels, value, ok := promLine(line)
		if !ok {
			continue
		}
		host := strings.ToLower(labels["host"])
		if host == "" || host == "_other" || strings.HasPrefix(host, "127.0.0.1") || host == "localhost" {
			continue
		}
		switch name {
		case "caddy_http_request_duration_seconds_count":
			a := get(host, labels["handler"])
			a.total += value
			if c := labels["code"]; len(c) == 3 && c[0] >= '1' && c[0] <= '5' {
				a.codes[c[:1]+"xx"] += value
			}
		case "caddy_http_request_duration_seconds_bucket":
			le := labels["le"]
			if le == "+Inf" {
				continue
			}
			if f, err := strconv.ParseFloat(le, 64); err == nil {
				get(host, labels["handler"]).buckets[f] += value
			}
		case "caddy_http_response_size_bytes_sum":
			get(host, labels["handler"]).bytes += value
		}
	}
	out := make([]api.ProxyHostStats, 0, len(byHost))
	for h, handlers := range byHost {
		var best *acc
		names := make([]string, 0, len(handlers))
		for n := range handlers {
			names = append(names, n)
		}
		sort.Strings(names) // a stable choice when two handlers tie
		for _, n := range names {
			if a := handlers[n]; best == nil || a.total > best.total {
				best = a
			}
		}
		s := api.ProxyHostStats{Host: h, Codes: best.codes, Bytes: best.bytes}
		les := make([]float64, 0, len(best.buckets))
		for le := range best.buckets {
			les = append(les, le)
		}
		sort.Float64s(les)
		for _, le := range les {
			s.Buckets = append(s.Buckets, api.ProxyBucket{Le: le, Count: best.buckets[le]})
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Host < out[j].Host })
	return out
}

// promLine splits one Prometheus text-format sample line.
func promLine(line string) (name string, labels map[string]string, value float64, ok bool) {
	labels = map[string]string{}
	rest := line
	if i := strings.IndexByte(line, '{'); i >= 0 {
		j := strings.LastIndexByte(line, '}')
		if j < i {
			return "", nil, 0, false
		}
		name = line[:i]
		for _, kv := range splitLabels(line[i+1 : j]) {
			k, v, found := strings.Cut(kv, "=")
			if found {
				labels[k] = strings.Trim(v, `"`)
			}
		}
		rest = strings.TrimSpace(line[j+1:])
	} else {
		f := strings.Fields(line)
		if len(f) < 2 {
			return "", nil, 0, false
		}
		name, rest = f[0], f[1]
	}
	if f := strings.Fields(rest); len(f) > 0 {
		rest = f[0]
	}
	v, err := strconv.ParseFloat(rest, 64)
	if err != nil {
		return "", nil, 0, false
	}
	return name, labels, v, true
}

// splitLabels splits `a="x",b="y,z"` on commas outside quotes.
func splitLabels(s string) []string {
	var out []string
	inQ, start := false, 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case '"':
			inQ = !inQ
		case ',':
			if !inQ {
				out = append(out, s[start:i])
				start = i + 1
			}
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}
