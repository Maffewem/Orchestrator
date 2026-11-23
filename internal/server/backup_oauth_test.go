package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Maffewem/Orchestrator/internal/store"
)

func TestBackupOAuthConnect(t *testing.T) {
	var gotForm url.Values
	idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			r.ParseForm()
			gotForm = r.PostForm
			json.NewEncoder(w).Encode(map[string]any{"access_token": "AT", "refresh_token": "RT", "expires_in": 3600, "token_type": "Bearer"})
		case "/drive":
			if r.Header.Get("Authorization") != "Bearer AT" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			json.NewEncoder(w).Encode(map[string]string{"id": "DRIVE1", "driveType": "personal"})
		}
	}))
	defer idp.Close()
	h := newHarness(t)
	h.srv.oauthOverride = map[string][2]string{"google": {idp.URL + "/auth", idp.URL + "/token"}, "onedrive": {idp.URL + "/auth", idp.URL + "/token"}}
	h.srv.graphURL = idp.URL + "/drive"

	// Starting without credentials is refused; secrets are never returned.
	if code := h.do("POST", "/api/backup-oauth/google/start", adminToken, nil, nil); code != http.StatusConflict {
		t.Fatalf("start without client: %d", code)
	}
	var views []oauthProviderView
	body := map[string]oauthClient{"google": {ClientID: "cid", ClientSecret: "sec"}, "onedrive": {ClientID: "oid", ClientSecret: "osec"}}
	if code := h.do("PUT", "/api/backup-oauth", adminToken, body, &views); code != http.StatusOK || len(views) != 3 || !views[0].SecretSet {
		t.Fatalf("save: %d %+v", code, views)
	}

	connect := func(provider string) {
		var start map[string]string
		if code := h.do("POST", "/api/backup-oauth/"+provider+"/start", adminToken, nil, &start); code != http.StatusOK {
			t.Fatalf("start: %d", code)
		}
		u, _ := url.Parse(start["url"])
		q := u.Query()
		if u.Host != strings.TrimPrefix(idp.URL, "http://") || q.Get("client_id") == "" || !strings.HasSuffix(q.Get("redirect_uri"), "/api/backup-oauth/callback") {
			t.Fatalf("bad auth url %s", start["url"])
		}
		client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		resp, err := client.Get(h.ts.URL + "/api/backup-oauth/callback?code=CODE&state=" + url.QueryEscape(q.Get("state")))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if loc := resp.Header.Get("Location"); !strings.Contains(loc, "cloud=") || strings.Contains(loc, "cloud_error") {
			t.Fatalf("callback redirect %q", loc)
		}
		// The state is single-use.
		resp, _ = client.Get(h.ts.URL + "/api/backup-oauth/callback?code=CODE&state=" + url.QueryEscape(q.Get("state")))
		resp.Body.Close()
		if !strings.Contains(resp.Header.Get("Location"), "cloud_error") {
			t.Fatal("state accepted twice")
		}
	}
	connect("google")
	if gotForm.Get("client_secret") != "sec" || gotForm.Get("code") != "CODE" {
		t.Fatalf("token request %v", gotForm)
	}
	connect("onedrive")

	dests := h.srv.backupDests(context.Background())
	if len(dests) != 2 || dests[0].Kind != "rclone" || !dests[0].Enabled {
		t.Fatalf("%+v", dests)
	}
	if c := dests[0].RcloneConf; !strings.Contains(c, "type = drive") || !strings.Contains(c, "refresh_token") || !strings.Contains(c, "scope = drive.file") {
		t.Fatalf("google conf: %s", c)
	}
	if c := dests[1].RcloneConf; !strings.Contains(c, "drive_id = DRIVE1") || !strings.Contains(c, "drive_type = personal") {
		t.Fatalf("onedrive conf: %s", c)
	}
	// An unknown or expired state is refused without touching destinations.
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, _ := client.Get(h.ts.URL + "/api/backup-oauth/callback?code=x&state=bogus")
	resp.Body.Close()
	if len(h.srv.backupDests(context.Background())) != 2 {
		t.Fatal("destination created from a bogus state")
	}
}

func TestBackupOAuthNeedsAdmin(t *testing.T) {
	h := newHarness(t)
	if code := h.do("GET", "/api/backup-oauth", "", nil, nil); code != http.StatusUnauthorized && code != http.StatusForbidden {
		t.Fatalf("anonymous read: %d", code)
	}
}

func TestStorageLevel(t *testing.T) {
	const g = int64(1 << 30)
	cases := []struct {
		backup, free, total int64
		want                string
	}{
		{1 * g, 50 * g, 100 * g, "ok"},
		{70 * g, 10 * g, 100 * g, "warning"}, // 87%
		{80 * g, 5 * g, 100 * g, "critical"}, // 94%
		{10 * g, 2 * g, 100 * g, "ok"},       // disk full, but not because of backups
		{30 * g, 0, 100 * g, "critical"},
	}
	for _, c := range cases {
		if got := storageLevel(c.backup, c.free, c.total); got != c.want {
			t.Errorf("%d/%d/%d: got %s want %s", c.backup/g, c.free/g, c.total/g, got, c.want)
		}
	}
}

func TestConsolidationVictims(t *testing.T) {
	day := func(d int) *store.Backup {
		return &store.Backup{ID: "b" + time.Date(2026, 11, d, 2, 0, 0, 0, time.UTC).Format("0102"), CreatedAt: time.Date(2026, 11, d, 2, 0, 0, 0, time.UTC)}
	}
	// Nov 2026: 1st is a Sunday, so ISO weeks start 2, 9, 16, 23.
	var list []*store.Backup
	for d := 23; d >= 1; d-- { // newest first
		list = append(list, day(d))
	}
	got := map[string]bool{}
	for _, id := range consolidationVictims(list, 3, 2) {
		got[id] = true
	}
	for _, id := range []string{"b1123", "b1122", "b1121"} {
		if got[id] {
			t.Errorf("daily backup %s removed", id)
		}
	}
	survivors := 0
	for _, b := range list {
		if !got[b.ID] {
			survivors++
		}
	}
	if survivors != 3+2 {
		t.Errorf("survivors = %d, want 5 (3 daily + 2 weekly)", survivors)
	}
	if got["b1115"] || got["b1108"] { // Sundays: newest backup of the prior weeks
		t.Errorf("weekly survivors missing: %v", got)
	}
}
