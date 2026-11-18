package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/Maffewem/Orchestrator/internal/api"
	"github.com/Maffewem/Orchestrator/internal/store"
)

// Project metrics: deployment history for every project, and hourly Worker analytics kept from
// Cloudflare so the Metrics tab can show more than the last 24 hours.

type deployPoint struct {
	Time      int64 `json:"t"`
	Succeeded int   `json:"succeeded"`
	Failed    int   `json:"failed"`
}

type deployStats struct {
	Total     int           `json:"total"`
	Succeeded int           `json:"succeeded"`
	Failed    int           `json:"failed"`
	Previews  int           `json:"previews"`
	Rollbacks int           `json:"rollbacks"`
	Median    *float64      `json:"median_seconds,omitempty"` // queued to finished, successful ones
	P95       *float64      `json:"p95_seconds,omitempty"`
	Series    []deployPoint `json:"series"`
}

type projectWorker struct {
	DeviceID   string `json:"device_id"`
	DeviceName string `json:"device_name"`
	Name       string `json:"name"`
	State      string `json:"state"`
}

type cfPoint struct {
	Time        int64    `json:"t"`
	Requests    int64    `json:"requests"`
	Errors      int64    `json:"errors"`
	Subrequests int64    `json:"subrequests"`
	CPUP50      *float64 `json:"cpu_p50,omitempty"` // milliseconds
	CPUP99      *float64 `json:"cpu_p99,omitempty"`
	WallP50     *float64 `json:"wall_p50,omitempty"`
	WallP99     *float64 `json:"wall_p99,omitempty"`
}

type cfMetricsView struct {
	Kind      string           `json:"kind"`
	Analytics string           `json:"analytics"`      // as in cfStatus
	Note      string           `json:"note,omitempty"` // why some figures are missing
	Series    []cfPoint        `json:"series"`
	Totals    cfPoint          `json:"totals"`
	ByStatus  map[string]int64 `json:"by_status"`
	ByVersion map[string]int64 `json:"by_version"`
}

type projectMetricsView struct {
	Kind        string          `json:"kind"` // device, compose or cloudflare
	Range       string          `json:"range"`
	Step        int64           `json:"step"`
	Workers     []projectWorker `json:"workers"`
	Deployments deployStats     `json:"deployments"`
	Cloudflare  *cfMetricsView  `json:"cloudflare,omitempty"`
}

func percentile(sorted []float64, q float64) *float64 {
	if len(sorted) == 0 {
		return nil
	}
	i := int(math.Ceil(q*float64(len(sorted)))) - 1
	v := sorted[max(0, min(i, len(sorted)-1))]
	return &v
}

func (s *Server) deploymentStats(ctx context.Context, projectID string, since, step int64) (deployStats, error) {
	st := deployStats{Series: []deployPoint{}}
	deps, err := s.store.ListDeployments(ctx, store.DeploymentFilter{ProjectID: projectID, Preview: "*", Limit: 500})
	if err != nil {
		return st, err
	}
	buckets := map[int64]*deployPoint{}
	var durations []float64
	for _, d := range deps {
		at := d.CreatedAt.Unix()
		if at < since {
			continue
		}
		st.Total++
		if d.PreviewID != "" {
			st.Previews++
		}
		if d.Trigger == store.TriggerRollback {
			st.Rollbacks++
		}
		b := (at / step) * step
		if buckets[b] == nil {
			buckets[b] = &deployPoint{Time: b}
		}
		switch d.Status {
		case "succeeded":
			st.Succeeded++
			buckets[b].Succeeded++
			if d.FinishedAt != nil {
				durations = append(durations, d.FinishedAt.Sub(d.CreatedAt).Seconds())
			}
		case "failed":
			st.Failed++
			buckets[b].Failed++
		}
	}
	for _, p := range buckets {
		st.Series = append(st.Series, *p)
	}
	sort.Slice(st.Series, func(i, j int) bool { return st.Series[i].Time < st.Series[j].Time })
	sort.Float64s(durations)
	st.Median, st.P95 = percentile(durations, 0.5), percentile(durations, 0.95)
	return st, nil
}

func (s *Server) handleProjectMetrics(w http.ResponseWriter, r *http.Request) {
	p, ok := s.lookupProject(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	rng, since, step := metricsRange(r)
	v := projectMetricsView{Kind: "device", Range: rng, Step: step, Workers: []projectWorker{}}
	var err error
	if v.Deployments, err = s.deploymentStats(ctx, p.ID, since, step); err != nil {
		s.internal(w, err)
		return
	}
	switch p.Source.Type {
	case store.SourceCloudflare:
		v.Kind = "cloudflare"
		cf, err := s.cloudflareMetrics(ctx, p, since, step)
		if err != nil {
			s.internal(w, err)
			return
		}
		v.Cloudflare = cf
	default:
		if p.Source.Type == store.SourceCompose {
			v.Kind = "compose"
		}
		d, err := s.store.GetDevice(ctx, p.DeviceID)
		if err == nil {
			projects, _ := s.store.ListProjects(ctx)
			for _, c := range d.Containers {
				if c.Labels["orchestrator.preview"] != "" {
					continue
				}
				if o := containerOwner(projects, d, c); o != nil && o.ID == p.ID {
					v.Workers = append(v.Workers, projectWorker{DeviceID: d.ID, DeviceName: d.Name, Name: c.Name, State: c.State})
				}
			}
		}
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) cloudflareMetrics(ctx context.Context, p *store.Project, since, step int64) (*cfMetricsView, error) {
	v := &cfMetricsView{Series: []cfPoint{}, ByStatus: map[string]int64{}, ByVersion: map[string]int64{}, Analytics: "none"}
	if p.Source.Cloudflare != nil {
		v.Kind = p.Source.Cloudflare.Kind
	}
	if st := s.cfStatusOf(ctx, p.ID); st != nil {
		v.Analytics = st.Analytics
		v.Note = st.MetricsNote
	}
	if v.Kind != api.CloudflareWorker {
		return v, nil
	}
	rows, err := s.store.CFSamples(ctx, p.ID, since)
	if err != nil {
		return nil, err
	}
	// Hourly rows go into buckets of step seconds; percentiles are weighted by requests.
	type agg struct {
		p                  cfPoint
		c50, c99, w50, w99 float64
		cw, ww             float64
	}
	buckets := map[int64]*agg{}
	var tot agg
	add := func(a *agg, r store.CFSample) {
		a.p.Requests += r.Requests
		a.p.Errors += r.Errors
		a.p.Subrequests += r.Subrequests
		wt := float64(max(r.Requests, 1))
		if r.CPUP50 != nil && r.CPUP99 != nil {
			a.c50 += *r.CPUP50 * wt
			a.c99 += *r.CPUP99 * wt
			a.cw += wt
		}
		if r.WallP50 != nil && r.WallP99 != nil {
			a.w50 += *r.WallP50 * wt
			a.w99 += *r.WallP99 * wt
			a.ww += wt
		}
	}
	finish := func(a *agg) cfPoint {
		ms := func(us, w float64) *float64 { v := math.Round(us/w/100) / 10; return &v } // µs to ms, 1 decimal
		if a.cw > 0 {
			a.p.CPUP50, a.p.CPUP99 = ms(a.c50, a.cw), ms(a.c99, a.cw)
		}
		if a.ww > 0 {
			a.p.WallP50, a.p.WallP99 = ms(a.w50, a.ww), ms(a.w99, a.ww)
		}
		return a.p
	}
	for _, r := range rows {
		b := (r.Time / step) * step
		if buckets[b] == nil {
			buckets[b] = &agg{p: cfPoint{Time: b}}
		}
		add(buckets[b], r)
		add(&tot, r)
	}
	keys := make([]int64, 0, len(buckets))
	for k := range buckets {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		v.Series = append(v.Series, finish(buckets[k]))
	}
	v.Totals = finish(&tot)
	if v.ByStatus, err = s.store.CFBreakdown(ctx, p.ID, "status", since); err != nil {
		return nil, err
	}
	if v.ByVersion, err = s.store.CFBreakdown(ctx, p.ID, "version", since); err != nil {
		return nil, err
	}
	return v, nil
}

// --- collecting hourly Worker analytics ---

// cfGraphQL runs one Cloudflare GraphQL Analytics query and decodes data into out.
func (s *Server) cfGraphQL(ctx context.Context, tok, query string, vars map[string]any, out any) error {
	b, _ := json.Marshal(map[string]any{"query": query, "variables": vars})
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.cloudflareAPI+"/graphql", bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var env struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&env); err != nil {
		return fmt.Errorf("analytics answered with status %s", resp.Status)
	}
	if len(env.Errors) > 0 {
		return errors.New(env.Errors[0].Message)
	}
	return json.Unmarshal(env.Data, out)
}

type cfHourRows struct {
	Viewer struct {
		Accounts []struct {
			Rows []struct {
				Dimensions struct {
					Hour    string `json:"datetimeHour"`
					Status  string `json:"status"`
					Version string `json:"scriptVersion"`
				} `json:"dimensions"`
				Sum struct {
					Requests    int64 `json:"requests"`
					Errors      int64 `json:"errors"`
					Subrequests int64 `json:"subrequests"`
				} `json:"sum"`
				Quantiles map[string]float64 `json:"quantiles"`
			} `json:"rows"`
		} `json:"accounts"`
	} `json:"viewer"`
}

const cfHourlyQuery = `query($acct: String!, $script: String!, $since: Time!, $until: Time!) {
  viewer { accounts(filter: {accountTag: $acct}) {
    rows: workersInvocationsAdaptive(limit: 10000, filter: {scriptName: $script, datetime_geq: $since, datetime_leq: $until}) {
      %s
    }
  } }
}`

func (s *Server) cfHourly(ctx context.Context, tok, acct, script string, since, until time.Time, fields string) (*cfHourRows, error) {
	var out cfHourRows
	err := s.cfGraphQL(ctx, tok, fmt.Sprintf(cfHourlyQuery, fields), map[string]any{"acct": acct, "script": script,
		"since": since.UTC().Format(time.RFC3339), "until": until.UTC().Format(time.RFC3339)}, &out)
	return &out, err
}

func hourOf(s string) (int64, bool) {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return 0, false
	}
	return t.Unix(), true
}

// cfCollectHourly stores a Worker's analytics by hour between since and until, as the
// project's metric preferences allow. It returns a note naming any figures Cloudflare refused.
func (s *Server) cfCollectHourly(ctx context.Context, p *store.Project, tok, acct, script string, since, until time.Time) string {
	pf := s.metricPrefs(ctx, p.ID, true)
	var notes []string
	hours := map[int64]*store.CFSample{}
	hour := func(ts int64) *store.CFSample {
		if hours[ts] == nil {
			hours[ts] = &store.CFSample{Time: ts}
		}
		return hours[ts]
	}
	// Requests by hour and invocation status: the totals and the status split come from here.
	status := map[int64]map[string]int64{}
	if pf.on(mCFRequests) || pf.on(mCFByStatus) {
		rows, err := s.cfHourly(ctx, tok, acct, script, since, until, "dimensions { datetimeHour status }\n      sum { requests errors subrequests }")
		if err != nil {
			return "Hourly requests unavailable: " + err.Error()
		}
		for _, a := range rows.Viewer.Accounts {
			for _, r := range a.Rows {
				ts, ok := hourOf(r.Dimensions.Hour)
				if !ok {
					continue
				}
				h := hour(ts)
				h.Requests += r.Sum.Requests
				h.Errors += r.Sum.Errors
				h.Subrequests += r.Sum.Subrequests
				if status[ts] == nil {
					status[ts] = map[string]int64{}
				}
				status[ts][r.Dimensions.Status] += r.Sum.Requests
			}
		}
	}
	// CPU and wall time percentiles by hour. Wall time is asked for separately so an account
	// whose schema lacks it still gets CPU time.
	var qf []string
	if pf.on(mCFCPUTime) {
		qf = append(qf, "cpuTimeP50 cpuTimeP99")
	}
	if pf.on(mCFWallTime) {
		qf = append(qf, "wallTimeP50 wallTimeP99")
	}
	for len(qf) > 0 {
		rows, err := s.cfHourly(ctx, tok, acct, script, since, until, "dimensions { datetimeHour }\n      quantiles { "+strings.Join(qf, " ")+" }")
		if err != nil {
			if len(qf) == 2 && strings.Contains(strings.ToLower(err.Error()), "wall") {
				notes = append(notes, "wall time is not offered for this account")
				qf = qf[:1]
				continue
			}
			notes = append(notes, "time percentiles unavailable: "+err.Error())
			break
		}
		for _, a := range rows.Viewer.Accounts {
			for _, r := range a.Rows {
				ts, ok := hourOf(r.Dimensions.Hour)
				if !ok {
					continue
				}
				h := hour(ts)
				if v, ok := r.Quantiles["cpuTimeP50"]; ok {
					h.CPUP50, h.CPUP99 = f64(v), f64(r.Quantiles["cpuTimeP99"])
				}
				if v, ok := r.Quantiles["wallTimeP50"]; ok {
					h.WallP50, h.WallP99 = f64(v), f64(r.Quantiles["wallTimeP99"])
				}
			}
		}
		break
	}
	version := map[int64]map[string]int64{}
	if pf.on(mCFByVersion) {
		rows, err := s.cfHourly(ctx, tok, acct, script, since, until, "dimensions { datetimeHour scriptVersion }\n      sum { requests }")
		if err != nil {
			notes = append(notes, "requests by version unavailable: "+err.Error())
		} else {
			for _, a := range rows.Viewer.Accounts {
				for _, r := range a.Rows {
					ts, ok := hourOf(r.Dimensions.Hour)
					if !ok {
						continue
					}
					if version[ts] == nil {
						version[ts] = map[string]int64{}
					}
					key := r.Dimensions.Version
					if key == "" {
						key = "unknown"
					}
					version[ts][key] += r.Sum.Requests
				}
			}
		}
	}
	for ts, h := range hours {
		if !pf.on(mCFRequests) {
			h.Requests, h.Errors, h.Subrequests = 0, 0, 0
		}
		if err := s.store.PutCFSample(ctx, p.ID, *h); err != nil {
			s.log.Warn("store cloudflare sample", "project", p.ID, "err", err)
		}
		if pf.on(mCFByStatus) && status[ts] != nil {
			s.store.PutCFBreakdown(ctx, p.ID, ts, "status", status[ts])
		}
	}
	for ts, m := range version {
		s.store.PutCFBreakdown(ctx, p.ID, ts, "version", m)
	}
	s.store.PruneCF(ctx, p.ID, time.Now().Add(-time.Duration(pf.RetentionDays)*24*time.Hour).Unix())
	if len(notes) == 0 {
		return ""
	}
	n := strings.Join(notes, "; ")
	return strings.ToUpper(n[:1]) + n[1:]
}

// cfCollectWorker keeps a Worker's hourly analytics up to date: the last three hours on every
// poll, and the last seven days the first time.
func (s *Server) cfCollectWorker(ctx context.Context, p *store.Project, tok, acct, script string) string {
	now := time.Now().UTC()
	if !s.store.HasCFSamples(ctx, p.ID) {
		var note string
		for day := 7; day >= 1; day-- { // one day per query keeps each answer small
			from := now.Truncate(time.Hour).Add(-time.Duration(day) * 24 * time.Hour)
			if n := s.cfCollectHourly(ctx, p, tok, acct, script, from, from.Add(24*time.Hour-time.Second)); n != "" {
				note = n
			}
		}
		if note != "" {
			return note
		}
	}
	return s.cfCollectHourly(ctx, p, tok, acct, script, now.Truncate(time.Hour).Add(-2*time.Hour), now)
}
