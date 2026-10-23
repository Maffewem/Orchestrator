package server

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// fakeIDP is a tiny OpenID Connect and GitHub OAuth provider.
type fakeIDP struct {
	*httptest.Server
	nonce  string
	claims map[string]any
}

func newFakeIDP(t *testing.T) *fakeIDP {
	f := &fakeIDP{claims: map[string]any{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"issuer": f.URL, "authorization_endpoint": f.URL + "/authorize", "token_endpoint": f.URL + "/token"})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		if r.Form.Get("code_verifier") == "" || r.Form.Get("client_secret") != "shh" {
			http.Error(w, "bad", 400)
			return
		}
		c := map[string]any{"iss": f.URL, "aud": "cid", "exp": time.Now().Add(time.Hour).Unix(), "nonce": f.nonce}
		for k, v := range f.claims {
			c[k] = v
		}
		b, _ := json.Marshal(c)
		json.NewEncoder(w).Encode(map[string]string{"id_token": "x." + base64.RawURLEncoding.EncodeToString(b) + ".y"})
	})
	// GitHub
	mux.HandleFunc("/login/oauth/access_token", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"access_token": "gho_x"})
	})
	mux.HandleFunc("/user", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"id": 42, "login": "Octo-Cat"})
	})
	mux.HandleFunc("/user/orgs", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]map[string]string{{"login": "Maffewem"}})
	})
	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	return f
}

// signIn walks start, callback and exchange; it returns the callback's redirect target and the session token.
func signIn(t *testing.T, h *harness, f *fakeIDP, provider string) (string, string) {
	t.Helper()
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Get(h.ts.URL + "/api/auth/sso/" + provider + "/start")
	if err != nil {
		t.Fatal(err)
	}
	loc, _ := url.Parse(resp.Header.Get("Location"))
	if !strings.HasPrefix(loc.String(), f.URL) {
		t.Fatalf("start redirected to %s", loc)
	}
	f.nonce = loc.Query().Get("nonce")
	resp, err = c.Get(h.ts.URL + "/api/auth/sso/" + provider + "/callback?code=abc&state=" + url.QueryEscape(loc.Query().Get("state")))
	if err != nil {
		t.Fatal(err)
	}
	dest, _ := url.Parse(resp.Header.Get("Location"))
	code := dest.Query().Get("sso")
	if code == "" {
		return dest.Query().Get("sso_error"), ""
	}
	var sess session
	if c := h.do("POST", "/api/auth/sso/exchange", "", map[string]string{"code": code}, &sess); c != http.StatusOK {
		t.Fatalf("exchange: %d", c)
	}
	if c := h.do("POST", "/api/auth/sso/exchange", "", map[string]string{"code": code}, nil); c != http.StatusUnauthorized {
		t.Fatalf("code reused: %d", c)
	}
	return "", sess.Token
}

func TestSSO(t *testing.T) {
	h := newHarness(t)
	f := newFakeIDP(t)
	h.srv.ssoGitHubURL, h.srv.githubAPI = f.URL, f.URL

	cfg := SSOConfig{
		OIDC:   SSOOIDC{Enabled: true, Name: "Cloudflare Access", Issuer: f.URL, ClientID: "cid", ClientSecret: "shh", Domains: []string{"example.com"}, DefaultRole: "viewer", AdminGroup: "orch-admins"},
		GitHub: SSOGitHub{Enabled: true, ClientID: "gid", ClientSecret: "shh", Orgs: []string{"maffewem"}, DefaultRole: "pending"},
	}
	if code := h.do("PUT", "/api/settings/sso", adminToken, cfg, nil); code != http.StatusOK {
		t.Fatalf("save: %d", code)
	}
	var pv struct {
		Providers []ssoProvider `json:"providers"`
	}
	h.do("GET", "/api/auth/sso/providers", "", nil, &pv)
	if len(pv.Providers) != 2 || pv.Providers[1].Name != "Cloudflare Access" {
		t.Fatalf("providers: %+v", pv)
	}

	// OIDC: allowed domain, member of the admin group.
	f.claims = map[string]any{"sub": "u1", "email": "Ann@Example.com", "email_verified": true, "groups": []string{"orch-admins"}}
	errMsg, tok := signIn(t, h, f, "oidc")
	var me identity
	if errMsg != "" || h.do("GET", "/api/auth/me", tok, nil, &me) != http.StatusOK || me.Name != "ann" || me.Role != "admin" {
		t.Fatalf("oidc login: %q %+v", errMsg, me)
	}
	// The same subject signs in to the same account.
	_, tok2 := signIn(t, h, f, "oidc")
	var me2 identity
	h.do("GET", "/api/auth/me", tok2, nil, &me2)
	if me2.Name != "ann" {
		t.Fatalf("second login: %+v", me2)
	}
	// A different domain is refused.
	f.claims = map[string]any{"sub": "u2", "email": "bob@evil.org", "email_verified": true}
	if msg, _ := signIn(t, h, f, "oidc"); !strings.Contains(msg, "not in the organisation or domain") {
		t.Fatalf("evil domain: %q", msg)
	}
	// An unverified email does not count.
	f.claims = map[string]any{"sub": "u3", "email": "eve@example.com", "email_verified": false}
	if msg, _ := signIn(t, h, f, "oidc"); msg == "" {
		t.Fatal("unverified email accepted")
	}
	// A local account named like a provider user is not taken over.
	h.do("POST", "/api/users", adminToken, map[string]string{"username": "carol", "password": "correct horse", "role": "admin"}, nil)
	f.claims = map[string]any{"sub": "u4", "email": "carol@example.com", "email_verified": true}
	_, tok4 := signIn(t, h, f, "oidc")
	var me4 identity
	h.do("GET", "/api/auth/me", tok4, nil, &me4)
	if me4.Name == "carol" || me4.Role != "viewer" {
		t.Fatalf("takeover: %+v", me4)
	}

	// GitHub: new users wait for approval.
	if msg, tok := signIn(t, h, f, "github"); tok != "" || !strings.Contains(msg, "waiting for an administrator") {
		t.Fatalf("pending: %q", msg)
	}
	var users []struct {
		ID, Username string
		Disabled     bool
	}
	h.do("GET", "/api/users", adminToken, nil, &users)
	var id string
	for _, u := range users {
		if u.Username == "octo-cat" && u.Disabled {
			id = u.ID
		}
	}
	if id == "" {
		t.Fatalf("pending user not created: %+v", users)
	}
	h.do("PATCH", "/api/users/"+id, adminToken, map[string]any{"disabled": false}, nil)
	if msg, tok := signIn(t, h, f, "github"); tok == "" {
		t.Fatalf("approved user: %q", msg)
	}

	// Enabling a provider needs an allow-list.
	bad := cfg
	bad.GitHub.Orgs = nil
	if code := h.do("PUT", "/api/settings/sso", adminToken, bad, nil); code != http.StatusBadRequest {
		t.Fatalf("empty allow-list: %d", code)
	}
}
