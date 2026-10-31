package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Maffewem/Orchestrator/internal/api"
	"github.com/Maffewem/Orchestrator/internal/store"
)

const settingCFStatus = "cfstatus:" // + project ID: JSON cfStatus

// Cloudflare deployment states shown on the dashboard.
const (
	cfStateLive     = "live"
	cfStateBuilding = "building"
	cfStateFailed   = "failed"
	cfStateMissing  = "missing" // nothing deployed under this name yet
	cfStateError    = "error"   // Cloudflare could not be asked
)

// cfStatus is what the monitor last learned about a Cloudflare project.
type cfStatus struct {
	Kind    string   `json:"kind"`
	Name    string   `json:"name"`
	State   string   `json:"state"`
	Detail  string   `json:"detail,omitempty"`
	URL     string   `json:"url,omitempty"`
	Domains []string `json:"domains,omitempty"`
	// Latest is the newest deployment Cloudflare knows of.
	LatestID string `json:"latest_id,omitempty"`
	LatestAt string `json:"latest_at,omitempty"`
	// Request figures cover the last 24 hours and exist for Workers only: Cloudflare keeps no
	// request analytics for static Pages sites in its GraphQL API.
	Requests    int64   `json:"requests"`
	Errors      int64   `json:"errors"`
	Subrequests int64   `json:"subrequests"`
	CPUP50      float64 `json:"cpu_p50_us"`
	CPUP99      float64 `json:"cpu_p99_us"`
	// Analytics is "ok", "none" (not collected for this kind) or why it could not be read.
	Analytics string    `json:"analytics"`
	PolledAt  time.Time `json:"polled_at"`
	// Alerting is set while an event has been raised for a high error rate, so it is raised once.
	Alerting bool `json:"alerting,omitempty"`
}

func (s *Server) cfStatusOf(ctx context.Context, projectID string) *cfStatus {
	v, _ := s.store.GetSetting(ctx, settingCFStatus+projectID)
	if v == "" {
		return nil
	}
	var st cfStatus
	if json.Unmarshal([]byte(v), &st) != nil {
		return nil
	}
	return &st
}

func (s *Server) saveCFStatus(ctx context.Context, projectID string, st *cfStatus) {
	b, _ := json.Marshal(st)
	s.store.SetSetting(ctx, settingCFStatus+projectID, string(b))
}

// cfPollProject asks Cloudflare about one project and records the answer. It raises an event
// when a project starts failing or its Workers start erroring.
func (s *Server) cfPollProject(ctx context.Context, p *store.Project) *cfStatus {
	t := p.Source.Cloudflare
	if p.Source.Type != store.SourceCloudflare || t == nil {
		return nil
	}
	prev := s.cfStatusOf(ctx, p.ID)
	st := &cfStatus{Kind: t.Kind, Name: cfNameOf(p), Analytics: "none", PolledAt: time.Now().UTC()}
	tok, acct := s.cfDeployCreds(ctx)
	if tok == "" || acct == "" {
		st.State, st.Detail = cfStateError, "Cloudflare Pages & Workers is not connected"
	} else if t.Kind == api.CloudflarePages {
		s.cfPollPages(ctx, tok, acct, st)
	} else {
		s.cfPollWorker(ctx, tok, acct, st)
	}
	if prev != nil {
		st.Alerting = prev.Alerting
	}
	if st.Requests >= 50 && float64(st.Errors)/float64(st.Requests) > 0.05 {
		if !st.Alerting {
			s.event(ctx, "", "warning", "cloudflare.errors", fmt.Sprintf("%s: %d of %d Worker requests failed in the last 24 hours", p.Name, st.Errors, st.Requests))
		}
		st.Alerting = true
	} else {
		st.Alerting = false
	}
	if st.State == cfStateFailed && (prev == nil || prev.State != cfStateFailed || prev.LatestID != st.LatestID) {
		s.event(ctx, "", "error", "cloudflare.failed", fmt.Sprintf("%s: Cloudflare reports the latest deployment failed", p.Name))
	}
	s.saveCFStatus(ctx, p.ID, st)
	return st
}

func (s *Server) cfPollPages(ctx context.Context, tok, acct string, st *cfStatus) {
	pp, err := s.cfGetPagesProject(ctx, tok, acct, st.Name)
	switch {
	case cfNotFound(err):
		st.State, st.Detail = cfStateMissing, "No Pages project with this name yet; the first deployment creates it"
		return
	case err != nil:
		st.State, st.Detail = cfStateError, err.Error()
		return
	}
	if pp.Subdomain != "" {
		st.URL = "https://" + pp.Subdomain
	}
	st.Domains = pp.Domains
	d := pp.Latest
	if d == nil {
		st.State, st.Detail = cfStateMissing, "No deployments yet"
		return
	}
	st.LatestID, st.LatestAt = d.ID, d.CreatedOn
	switch d.Stage.Status {
	case "success":
		st.State = cfStateLive
	case "failure", "canceled":
		st.State, st.Detail = cfStateFailed, "Latest deployment "+d.Stage.Status+" at "+d.Stage.Name
	default:
		st.State, st.Detail = cfStateBuilding, "Latest deployment is at "+d.Stage.Name
	}
}

func (s *Server) cfPollWorker(ctx context.Context, tok, acct string, st *cfStatus) {
	ds, err := s.cfWorkerDeployments(ctx, tok, acct, st.Name)
	switch {
	case cfNotFound(err):
		st.State, st.Detail = cfStateMissing, "No Worker with this name yet; the first deployment creates it"
		return
	case err != nil:
		st.State, st.Detail = cfStateError, err.Error()
		return
	}
	st.State = cfStateLive
	if len(ds) == 0 {
		st.State, st.Detail = cfStateMissing, "No deployments yet"
	} else {
		st.LatestID, st.LatestAt = ds[0].ID, ds[0].CreatedOn
	}
	var sub struct {
		Subdomain string `json:"subdomain"`
	}
	if s.cfCall(ctx, tok, http.MethodGet, fmt.Sprintf("/accounts/%s/workers/subdomain", acct), nil, &sub) == nil && sub.Subdomain != "" && st.State == cfStateLive {
		st.URL = fmt.Sprintf("https://%s.%s.workers.dev", st.Name, sub.Subdomain)
	}
	stats, err := s.cfWorkerAnalytics(ctx, tok, acct, st.Name, time.Now())
	if err != nil {
		st.Analytics = "unavailable: " + err.Error()
		return
	}
	st.Analytics = "ok"
	st.Requests, st.Errors, st.Subrequests, st.CPUP50, st.CPUP99 = stats.Requests, stats.Errors, stats.Subrequests, stats.CPUP50, stats.CPUP99
}

// cfPollAll refreshes every Cloudflare project.
func (s *Server) cfPollAll(ctx context.Context) {
	if !s.cfDeployReady(ctx) {
		return
	}
	projects, err := s.store.ListProjects(ctx)
	if err != nil {
		s.log.Error("cloudflare monitor: list projects", "err", err)
		return
	}
	for _, p := range projects {
		if p.Source.Type == store.SourceCloudflare {
			s.cfPollProject(ctx, p)
		}
	}
}

func (s *Server) cfMonitorLoop(ctx context.Context) {
	if s.cfg.CloudflareInterval <= 0 {
		return
	}
	t := time.NewTicker(s.cfg.CloudflareInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.cfPollAll(ctx)
		}
	}
}

// recoverCloudflare fails deployments that were running when the server last stopped: no one is carrying them out now.
func (s *Server) recoverCloudflare(ctx context.Context) {
	ids, err := s.store.UnfinishedLocal(ctx)
	if err != nil {
		s.log.Error("cloudflare: recover", "err", err)
		return
	}
	for _, id := range ids {
		s.store.FinishDeployment(ctx, id, false, "The server restarted while this deployment was running. Retry it.")
	}
}

func (s *Server) handleGetCloudflare(w http.ResponseWriter, r *http.Request) {
	p, ok := s.lookupProject(w, r)
	if !ok {
		return
	}
	if p.Source.Type != store.SourceCloudflare {
		writeError(w, http.StatusBadRequest, "this project is not deployed to Cloudflare")
		return
	}
	writeJSON(w, http.StatusOK, s.cfStatusOf(r.Context(), p.ID))
}

// handleRefreshCloudflare polls Cloudflare now instead of waiting for the next round.
func (s *Server) handleRefreshCloudflare(w http.ResponseWriter, r *http.Request) {
	p, ok := s.lookupProject(w, r)
	if !ok {
		return
	}
	st := s.cfPollProject(r.Context(), p)
	if st == nil {
		writeError(w, http.StatusBadRequest, "this project is not deployed to Cloudflare")
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// cfSummary is the fleet-wide Cloudflare line on the overview.
type cfSummary struct {
	Projects int   `json:"projects"`
	Live     int   `json:"live"`
	Failing  int   `json:"failing"`
	Requests int64 `json:"requests"`
	Errors   int64 `json:"errors"`
}

func (s *Server) cfOverview(ctx context.Context) cfSummary {
	var sum cfSummary
	projects, _ := s.store.ListProjects(ctx)
	for _, p := range projects {
		if p.Source.Type != store.SourceCloudflare {
			continue
		}
		sum.Projects++
		if st := s.cfStatusOf(ctx, p.ID); st != nil {
			switch st.State {
			case cfStateLive:
				sum.Live++
			case cfStateFailed, cfStateError:
				sum.Failing++
			}
			sum.Requests += st.Requests
			sum.Errors += st.Errors
		}
	}
	return sum
}

// handleSetCloudflareDeploy stores the account ID (and optionally a token only for deployments)
// after checking they can reach Pages and Workers. An empty account ID disconnects.
func (s *Server) handleSetCloudflareDeploy(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AccountID string `json:"account_id"`
		Token     string `json:"token"`
	}
	if !decode(w, r, &req) {
		return
	}
	ctx := r.Context()
	acct := strings.TrimSpace(req.AccountID)
	if acct == "" {
		s.store.SetSetting(ctx, settingCFAccount, "")
		s.store.SetSetting(ctx, settingCFDeployToken, "")
		s.event(ctx, "", "info", "settings.cloudflare", "Cloudflare Pages & Workers disconnected")
		writeJSON(w, http.StatusOK, s.cloudflareStatus(ctx))
		return
	}
	if strings.ContainsAny(acct, "/? #") {
		writeError(w, http.StatusBadRequest, "the account ID is the 32-character code from the Cloudflare dashboard")
		return
	}
	tok := strings.TrimSpace(req.Token)
	if tok == "" {
		tok, _ = s.store.GetSetting(ctx, settingCFDeployToken)
	}
	if tok == "" {
		tok = s.cloudflareToken(ctx)
	}
	if tok == "" {
		writeError(w, http.StatusBadRequest, "add an API token: either connect Cloudflare above or paste a token made for Pages and Workers")
		return
	}
	var pages, workers bool
	var firstErr error
	var junk json.RawMessage
	for _, c := range []struct {
		ok   *bool
		path string
	}{{&pages, "/accounts/" + acct + "/pages/projects?per_page=1"}, {&workers, "/accounts/" + acct + "/workers/subdomain"}} {
		if err := s.cfCall(ctx, tok, http.MethodGet, c.path, nil, &junk); err != nil {
			if firstErr == nil || cfHTTPStatus(err) == http.StatusUnauthorized {
				firstErr = err
			}
		} else {
			*c.ok = true
		}
	}
	if !pages && !workers {
		msg := strings.TrimPrefix(firstErr.Error(), "cloudflare: ")
		writeError(w, http.StatusBadRequest, "Cloudflare would not let this token use account "+acct+": "+msg+". The token needs Account > Cloudflare Pages > Edit and Account > Workers Scripts > Edit, and Account > Account Analytics > Read to show request counts")
		return
	}
	s.store.SetSetting(ctx, settingCFAccount, acct)
	if strings.TrimSpace(req.Token) != "" {
		s.store.SetSetting(ctx, settingCFDeployToken, tok)
	}
	s.event(ctx, "", "info", "settings.cloudflare", "Cloudflare Pages & Workers connected")
	st := s.cloudflareStatus(ctx)
	st["pages"], st["workers"] = pages, workers
	writeJSON(w, http.StatusOK, st)
}

func cfHTTPStatus(err error) int {
	var ce *cfError
	if errors.As(err, &ce) {
		return ce.Status
	}
	return 0
}
