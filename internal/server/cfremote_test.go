package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// serveRemote answers the read-only history, domain and settings endpoints. It reports whether
// it handled the request.
func (f *cfFake) serveRemote(w http.ResponseWriter, p string, r *http.Request, ok func(any), notFound func()) bool {
	for suffix := range f.remoteDenied {
		if strings.HasSuffix(p, suffix) {
			w.WriteHeader(http.StatusForbidden)
			json.NewEncoder(w).Encode(map[string]any{"success": false, "errors": []map[string]string{{"message": "Authentication error"}}})
			return true
		}
	}
	const wk = "/accounts/acct/workers/scripts/"
	const pg = "/accounts/acct/pages/projects/"
	handled := true
	switch {
	case strings.HasPrefix(p, pg) && strings.HasSuffix(p, "/deployments") && r.Method == http.MethodGet:
		ok([]any{
			map[string]any{"id": "d2", "short_id": "d2", "url": "https://d2.blog.pages.dev", "environment": "production", "created_on": "2026-11-02T10:00:00Z",
				"deployment_trigger": map[string]any{"type": "github:push", "metadata": map[string]any{"branch": "trunk", "commit_hash": "abcdef1234", "commit_message": "Fix header\n\nlong body"}},
				"latest_stage":       map[string]string{"name": "deploy", "status": "success"}},
			map[string]any{"id": "d3", "short_id": "d3", "url": "https://d3.blog.pages.dev", "environment": "preview", "created_on": "2026-11-03T10:00:00Z", "aliases": []string{"https://feat-x.blog.pages.dev"},
				"deployment_trigger": map[string]any{"type": "github:push", "metadata": map[string]any{"branch": "feat/x", "commit_hash": "1111111", "commit_message": "WIP"}},
				"latest_stage":       map[string]string{"name": "build", "status": "failure"}},
			map[string]any{"id": "d4", "short_id": "d4", "url": "https://d4.blog.pages.dev", "environment": "preview", "created_on": "2026-11-01T10:00:00Z",
				"deployment_trigger": map[string]any{"type": "ad_hoc", "metadata": map[string]any{"branch": "feat/x"}},
				"latest_stage":       map[string]string{"name": "deploy", "status": "success"}},
		})
	case strings.HasPrefix(p, pg) && strings.HasSuffix(p, "/domains"):
		ok([]any{map[string]string{"name": "www.example.com", "status": "active"}})
	case strings.HasPrefix(p, wk) && strings.HasSuffix(p, "/versions"):
		ok(map[string]any{"items": []any{
			map[string]any{"id": "aaaaaaaa-1111", "number": 2, "metadata": map[string]any{"created_on": "2026-11-05T10:00:00Z", "source": "wrangler", "author_email": "dev@example.com"}, "annotations": map[string]string{"workers/message": "Ship it\nmore"}},
			map[string]any{"id": "bbbbbbbb-2222", "number": 1, "metadata": map[string]any{"created_on": "2026-11-01T10:00:00Z"}, "annotations": map[string]string{}},
		}})
	case strings.HasPrefix(p, wk) && strings.HasSuffix(p, "/deployments"):
		name := strings.TrimSuffix(strings.TrimPrefix(p, wk), "/deployments")
		if _, found := f.workerUploads[name]; !found {
			notFound()
			return true
		}
		ok(map[string]any{"deployments": []any{
			map[string]any{"id": "w-deploy-1", "created_on": "2026-11-05T10:05:00Z", "source": "wrangler", "author_email": "dev@example.com",
				"annotations": map[string]string{}, "versions": []any{map[string]any{"version_id": "aaaaaaaa-1111", "percentage": 100}}},
			map[string]any{"id": "w-deploy-0", "created_on": "2026-11-01T10:05:00Z", "source": "dash",
				"versions": []any{map[string]any{"version_id": "bbbbbbbb-2222", "percentage": 100}}},
		}})
	case strings.HasPrefix(p, wk) && strings.HasSuffix(p, "/settings"):
		ok(map[string]any{"compatibility_date": "2026-01-15", "compatibility_flags": []string{"nodejs_compat"}, "observability": map[string]bool{"enabled": true},
			"bindings": []any{
				map[string]any{"name": "GREETING", "type": "plain_text", "text": "hello"},
				map[string]any{"name": "API_KEY", "type": "secret_text", "text": "super-secret-value"},
				map[string]any{"name": "KV", "type": "kv_namespace", "namespace_id": "ns123"},
				map[string]any{"name": "BUCKET", "type": "r2_bucket", "bucket_name": "assets"},
			}})
	case strings.HasPrefix(p, wk) && strings.HasSuffix(p, "/subdomain"):
		ok(map[string]bool{"enabled": true, "previews_enabled": true})
	case p == "/accounts/acct/workers/domains":
		if r.URL.Query().Get("service") != "aurora" {
			ok([]any{})
			return true
		}
		ok([]any{map[string]any{"hostname": "aurora.example.com", "zone_name": "example.com", "service": "aurora"}})
	case p == "/zones":
		ok([]any{map[string]string{"id": "z1", "name": "example.com"}})
	case p == "/zones/z1/workers/routes":
		ok([]any{map[string]string{"id": "r1", "pattern": "example.com/api/*", "script": "aurora"}, map[string]string{"id": "r2", "pattern": "other.com/*", "script": "else"}})
	default:
		handled = false
	}
	return handled
}

func TestCloudflareWorkerHistorySettingsAndDomains(t *testing.T) {
	f := newCFFake(t)
	f.workerUploads["aurora"] = "{}"
	h := newHarness(t)
	f.connect(h)
	h.do("PUT", "/api/settings/cloudflare/deploy", adminToken, map[string]string{"account_id": "acct", "token": "deploytok"}, nil)
	var res []cfImportResult
	h.do("POST", "/api/cloudflare/import", adminToken, map[string]any{"items": []map[string]string{{"kind": "worker", "name": "aurora"}}}, &res)
	if res[0].Project != "aurora" {
		t.Fatalf("import: %+v", res)
	}
	var projects []ProjectView
	h.do("GET", "/api/projects", adminToken, nil, &projects)
	id := projects[0].ID
	var rem cfRemote
	if code := h.do("GET", "/api/projects/"+id+"/cloudflare/remote", adminToken, nil, &rem); code != http.StatusOK {
		t.Fatalf("remote: %d", code)
	}
	if len(rem.Deployments) != 2 || !rem.Deployments[0].Live || rem.Deployments[0].Label != "v2" || rem.Deployments[0].Message != "Ship it" || rem.Deployments[0].Author != "dev@example.com" || rem.Deployments[1].Source != "Dashboard" || rem.Deployments[1].Live {
		t.Fatalf("deployments: %+v", rem.Deployments)
	}
	if len(rem.Previews) != 1 || rem.Previews[0].Label != "v1" || rem.Previews[0].URL != "https://bbbbbbbb-aurora.matt.workers.dev" {
		t.Fatalf("only versions that are not live are previews: %+v", rem.Previews)
	}
	if len(rem.Domains) != 2 || rem.Domains[0].Host != "aurora.example.com" || rem.Domains[1].Host != "example.com" || rem.Domains[1].Kind != "route" {
		t.Fatalf("domains: %+v", rem.Domains)
	}
	set := rem.Settings
	if set == nil || set.CompatibilityDate != "2026-01-15" || len(set.Bindings) != 2 || len(set.Variables) != 2 {
		t.Fatalf("settings: %+v", set)
	}
	raw, _ := json.Marshal(rem)
	if strings.Contains(string(raw), "super-secret-value") {
		t.Fatal("a secret value leaked into the stored Cloudflare data")
	}
	for _, v := range set.Variables {
		if v.Name == "API_KEY" && (!v.Secret || v.Value != "") {
			t.Fatalf("secret variable: %+v", v)
		}
	}
	// The custom domain wins as the address to visit; workers.dev is kept as the default.
	var st cfStatus
	h.do("GET", "/api/projects/"+id+"/cloudflare", adminToken, nil, &st)
	if st.URL != "https://aurora.example.com" || st.DefaultURL != "https://aurora.matt.workers.dev" {
		t.Fatalf("status urls: %q %q", st.URL, st.DefaultURL)
	}
}

func TestCloudflarePagesHistoryPreviewsAndMissingPermissions(t *testing.T) {
	f := newCFFake(t)
	f.pagesProjects["blog"] = "trunk"
	h := newHarness(t)
	f.connect(h)
	h.do("PUT", "/api/settings/cloudflare/deploy", adminToken, map[string]string{"account_id": "acct", "token": "deploytok"}, nil)
	var res []cfImportResult
	h.do("POST", "/api/cloudflare/import", adminToken, map[string]any{"items": []map[string]string{{"kind": "pages", "name": "blog"}}}, &res)
	var projects []ProjectView
	h.do("GET", "/api/projects", adminToken, nil, &projects)
	id := projects[0].ID
	var rem cfRemote
	h.do("GET", "/api/projects/"+id+"/cloudflare/remote", adminToken, nil, &rem)
	if len(rem.Deployments) != 3 || rem.Deployments[0].Message != "Fix header" || rem.Deployments[0].Source != "GitHub" || rem.Deployments[1].Status != "failure" {
		t.Fatalf("deployments: %+v", rem.Deployments)
	}
	if len(rem.Previews) != 1 || rem.Previews[0].Label != "feat/x" || rem.Previews[0].Status != "failure" || len(rem.Previews[0].Aliases) != 1 {
		t.Fatalf("previews keep the newest deployment per branch: %+v", rem.Previews)
	}
	if len(rem.Domains) != 1 || rem.Domains[0].Host != "www.example.com" || rem.Domains[0].Status != "active" {
		t.Fatalf("domains: %+v", rem.Domains)
	}
	var st cfStatus
	h.do("GET", "/api/projects/"+id+"/cloudflare", adminToken, nil, &st)
	if st.URL != "https://www.example.com" {
		t.Fatalf("visit link: %q", st.URL)
	}

	// A token without permission to list deployments gives a clear note, and the rest still loads.
	f.remoteDenied = map[string]bool{"/deployments": true}
	h.do("POST", "/api/projects/"+id+"/cloudflare/refresh", adminToken, nil, nil)
	h.do("GET", "/api/projects/"+id+"/cloudflare/remote", adminToken, nil, &rem)
	if !strings.Contains(rem.Notes["deployments"], "not allowed") || len(rem.Deployments) != 0 || rem.Settings == nil {
		t.Fatalf("notes: %+v settings: %+v", rem.Notes, rem.Settings)
	}
}
