package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Maffewem/Orchestrator/internal/api"
	"github.com/Maffewem/Orchestrator/internal/store"
)

func statsHeartbeat(h *harness, tok string, at int64, cs ...api.ContainerStats) {
	h.t.Helper()
	containers := []api.Container{}
	for _, c := range cs {
		containers = append(containers, api.Container{ID: c.ID, Name: c.Name, State: "running", Volumes: []string{"data"}})
	}
	req := api.HeartbeatRequest{Host: api.HostInfo{Capabilities: []string{api.CapStats}}, Containers: containers,
		Stats: &api.StatsReport{Time: at, Containers: cs, Volumes: map[string]int64{"data": 4096}}}
	if code := h.do("POST", "/api/agent/heartbeat", tok, req, nil); code != http.StatusOK {
		h.t.Fatalf("heartbeat: %d", code)
	}
}

func TestWorkerMetricsStoresRatesAtInterval(t *testing.T) {
	h := newHarness(t)
	reg := h.register("pi")
	now := time.Now().Unix() - 300
	c := api.ContainerStats{ID: "c1", Name: "web", CPUPercent: 10, MemUsed: 100 << 20, MemLimit: 512 << 20, NetRx: 1000, NetTx: 2000, PIDs: 5, Restarts: 0, Health: "healthy"}
	statsHeartbeat(h, reg.AgentToken, now, c) // first reading is the baseline
	c.CPUPercent, c.NetRx, c.NetTx = 30, 7000, 2000
	statsHeartbeat(h, reg.AgentToken, now+30, c) // under the 60 s default interval: accumulated only
	c.CPUPercent, c.NetRx, c.MemUsed, c.Restarts = 20, 13000, 150<<20, 1
	statsHeartbeat(h, reg.AgentToken, now+60, c)

	var v workerMetricsView
	if code := h.do("GET", "/api/devices/"+reg.DeviceID+"/workers/web/metrics?range=1h", adminToken, nil, &v); code != http.StatusOK {
		t.Fatalf("metrics: %d", code)
	}
	if !v.Supported || len(v.Series) != 1 {
		t.Fatalf("view: %+v", v)
	}
	s := v.Series[0]
	if s.CPU == nil || *s.CPU != 25 { // average of the two readings since the baseline
		t.Fatalf("cpu: %v", s.CPU)
	}
	if s.NetRx == nil || *s.NetRx != 200 { // (13000-1000) bytes over 60 s
		t.Fatalf("net rx: %v", *s.NetRx)
	}
	if s.Mem == nil || *s.Mem != 150<<20 || s.Restarts == nil || *s.Restarts != 1 {
		t.Fatalf("mem/restarts: %+v", s)
	}
	if v.Current == nil || v.Current.Health != "healthy" || len(v.Volumes) != 1 || v.Volumes[0].Size != 4096 {
		t.Fatalf("current/volumes: %+v %+v", v.Current, v.Volumes)
	}
}

func TestWorkerMetricPrefsSwitchOffColumns(t *testing.T) {
	h := newHarness(t)
	reg := h.register("pi")
	path := "/api/devices/" + reg.DeviceID + "/workers/web/metrics/prefs"
	if code := h.do("PUT", path, adminToken, metricPrefs{Metrics: []string{"bogus"}, Interval: 60, RetentionDays: 30}, nil); code != http.StatusBadRequest {
		t.Fatalf("bad metric accepted: %d", code)
	}
	if code := h.do("PUT", path, adminToken, metricPrefs{Metrics: []string{mMemory}, Interval: 15, RetentionDays: 7}, nil); code != http.StatusOK {
		t.Fatalf("save prefs: %d", code)
	}
	var pv metricPrefsView
	h.do("GET", path, adminToken, nil, &pv)
	if pv.Prefs.Interval != 15 || pv.Prefs.RetentionDays != 7 || len(pv.Prefs.Metrics) != 1 || pv.Storage <= 0 {
		t.Fatalf("prefs: %+v", pv)
	}
	now := time.Now().Unix() - 100
	c := api.ContainerStats{ID: "c1", Name: "web", CPUPercent: 50, MemUsed: 1 << 20}
	statsHeartbeat(h, reg.AgentToken, now, c)
	statsHeartbeat(h, reg.AgentToken, now+15, c)
	var v workerMetricsView
	h.do("GET", "/api/devices/"+reg.DeviceID+"/workers/web/metrics?range=1h", adminToken, nil, &v)
	if len(v.Series) != 1 || v.Series[0].CPU != nil || v.Series[0].Mem == nil {
		t.Fatalf("series: %+v", v.Series)
	}
}

func TestProxyDeltaAndQuantiles(t *testing.T) {
	base := api.ProxyHostStats{Codes: map[string]float64{"2xx": 10, "5xx": 1}, Bytes: 1000,
		Buckets: []api.ProxyBucket{{Le: 0.01, Count: 5}, {Le: 0.1, Count: 10}, {Le: 1, Count: 11}}}
	cur := api.ProxyHostStats{Codes: map[string]float64{"2xx": 110, "5xx": 1, "4xx": 4}, Bytes: 5000,
		Buckets: []api.ProxyBucket{{Le: 0.01, Count: 55}, {Le: 0.1, Count: 105}, {Le: 1, Count: 115}}}
	p := proxyDelta(base, cur)
	if p.Requests != 104 || p.C2xx != 100 || p.C4xx != 4 || *p.Bytes != 4000 {
		t.Fatalf("delta: %+v", p)
	}
	// 50 requests under 10 ms, 45 more under 100 ms, 9 more under 1 s.
	if len(p.Hist) != 3 || p.Hist[0][1] != 50 || p.Hist[1][1] != 45 || p.Hist[2][1] != 9 {
		t.Fatalf("hist: %v", p.Hist)
	}
	hist := map[float64]float64{}
	for _, b := range p.Hist {
		hist[b[0]] = b[1]
	}
	if q := histQuantile(hist, 0.5); q == nil || *q < 10 || *q > 100 {
		t.Fatalf("p50: %v", q)
	}
	// Caddy restarted: the counters are lower, so the new reading is all new traffic.
	reset := proxyDelta(cur, api.ProxyHostStats{Codes: map[string]float64{"2xx": 3}, Bytes: 10})
	if reset.Requests != 3 {
		t.Fatalf("reset: %+v", reset)
	}
}

func TestCloudflareHourlyCollection(t *testing.T) {
	h := newHarness(t)
	var queries []string
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var body struct {
			Query string `json:"query"`
		}
		json.Unmarshal(b, &body)
		queries = append(queries, body.Query)
		hour := time.Now().UTC().Truncate(time.Hour).Format(time.RFC3339)
		switch {
		case strings.Contains(body.Query, "wallTimeP50"):
			io.WriteString(w, `{"errors":[{"message":"unknown field wallTimeP50"}]}`)
		case strings.Contains(body.Query, "quantiles"):
			io.WriteString(w, `{"data":{"viewer":{"accounts":[{"rows":[{"dimensions":{"datetimeHour":"`+hour+`"},"quantiles":{"cpuTimeP50":1500,"cpuTimeP99":9000}}]}]}}}`)
		case strings.Contains(body.Query, "scriptVersion"):
			io.WriteString(w, `{"data":{"viewer":{"accounts":[{"rows":[{"dimensions":{"datetimeHour":"`+hour+`","scriptVersion":"v2"},"sum":{"requests":90}},{"dimensions":{"datetimeHour":"`+hour+`","scriptVersion":"v1"},"sum":{"requests":10}}]}]}}}`)
		default:
			io.WriteString(w, `{"data":{"viewer":{"accounts":[{"rows":[{"dimensions":{"datetimeHour":"`+hour+`","status":"success"},"sum":{"requests":95,"errors":0,"subrequests":40}},{"dimensions":{"datetimeHour":"`+hour+`","status":"scriptThrewException"},"sum":{"requests":5,"errors":5,"subrequests":0}}]}]}}}`)
		}
	}))
	defer fake.Close()
	h.srv.cloudflareAPI = fake.URL
	p := &store.Project{ID: "prj_cf", Source: store.Source{Type: store.SourceCloudflare, Cloudflare: &store.CloudflareTarget{Kind: api.CloudflareWorker, Name: "edge"}}}
	ctx := context.Background()
	now := time.Now()
	note := h.srv.cfCollectHourly(ctx, p, "tok", "acct", "edge", now.Add(-time.Hour), now)
	if !strings.Contains(strings.ToLower(note), "wall time") {
		t.Fatalf("note: %q", note)
	}
	v, err := h.srv.cloudflareMetrics(ctx, p, now.Add(-24*time.Hour).Unix(), 3600)
	if err != nil {
		t.Fatal(err)
	}
	if v.Totals.Requests != 100 || v.Totals.Errors != 5 || v.Totals.CPUP50 == nil || *v.Totals.CPUP50 != 1.5 || v.Totals.WallP50 != nil {
		t.Fatalf("totals: %+v", v.Totals)
	}
	if v.ByStatus["scriptThrewException"] != 5 || v.ByVersion["v2"] != 90 {
		t.Fatalf("breakdown: %+v %+v", v.ByStatus, v.ByVersion)
	}
	if len(queries) != 4 { // status, quantiles with wall time, quantiles without, versions
		t.Fatalf("queries: %d", len(queries))
	}
}
