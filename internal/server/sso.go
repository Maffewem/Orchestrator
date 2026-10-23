package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/Maffewem/Orchestrator/internal/store"
)

// Single sign-on through GitHub (OAuth) or any OpenID Connect provider. Cloudflare Access,
// Google, Microsoft Entra, Authentik and Keycloak all speak OIDC.
//
// The browser is sent to the provider and comes back to /api/auth/sso/{provider}/callback.
// The server then redirects to /?sso=<one-time code> and the dashboard swaps the code for a
// normal session token, so tokens never travel in a URL.

const settingSSO = "sso_config"

type SSOGitHub struct {
	Enabled      bool     `json:"enabled"`
	ClientID     string   `json:"client_id"`
	ClientSecret string   `json:"client_secret,omitempty"`
	Orgs         []string `json:"orgs"`
	DefaultRole  string   `json:"default_role"`
}

type SSOOIDC struct {
	Enabled       bool     `json:"enabled"`
	Name          string   `json:"name"`
	Issuer        string   `json:"issuer"`
	ClientID      string   `json:"client_id"`
	ClientSecret  string   `json:"client_secret,omitempty"`
	Domains       []string `json:"domains"` // allowed email domains, or "*" to trust the provider
	DefaultRole   string   `json:"default_role"`
	GroupClaim    string   `json:"group_claim"`
	AdminGroup    string   `json:"admin_group"`
	OperatorGroup string   `json:"operator_group"`
}

type SSOConfig struct {
	GitHub       SSOGitHub `json:"github"`
	OIDC         SSOOIDC   `json:"oidc"`
	HidePassword bool      `json:"hide_password"`
}

type ssoView struct {
	SSOConfig
	GitHubSecretSet bool `json:"github_secret_set"`
	OIDCSecretSet   bool `json:"oidc_secret_set"`
}

func (s *Server) ssoConfig(ctx context.Context) SSOConfig {
	var c SSOConfig
	if v, err := s.store.GetSetting(ctx, settingSSO); err == nil {
		json.Unmarshal([]byte(v), &c)
	}
	return c
}

func (s *Server) ssoView(ctx context.Context) ssoView {
	c := s.ssoConfig(ctx)
	v := ssoView{SSOConfig: c, GitHubSecretSet: c.GitHub.ClientSecret != "", OIDCSecretSet: c.OIDC.ClientSecret != ""}
	v.GitHub.ClientSecret, v.OIDC.ClientSecret = "", ""
	return v
}

func cleanList(in []string) []string {
	out := []string{}
	for _, x := range in {
		if x = strings.ToLower(strings.TrimSpace(x)); x != "" {
			out = append(out, x)
		}
	}
	return out
}

func validRole(r string) bool {
	return r == "pending" || r == store.RoleViewer || r == store.RoleOperator
}

func (s *Server) handleSetSSO(w http.ResponseWriter, r *http.Request) {
	var req SSOConfig
	if !decode(w, r, &req) {
		return
	}
	old := s.ssoConfig(r.Context())
	if req.GitHub.ClientSecret == "" {
		req.GitHub.ClientSecret = old.GitHub.ClientSecret
	}
	if req.OIDC.ClientSecret == "" {
		req.OIDC.ClientSecret = old.OIDC.ClientSecret
	}
	g, o := &req.GitHub, &req.OIDC
	g.Orgs, o.Domains = cleanList(g.Orgs), cleanList(o.Domains)
	g.ClientID, o.ClientID = strings.TrimSpace(g.ClientID), strings.TrimSpace(o.ClientID)
	o.Issuer = strings.TrimRight(strings.TrimSpace(o.Issuer), "/")
	if g.DefaultRole == "" {
		g.DefaultRole = "pending"
	}
	if o.DefaultRole == "" {
		o.DefaultRole = "pending"
	}
	if o.GroupClaim == "" {
		o.GroupClaim = "groups"
	}
	if !validRole(g.DefaultRole) || !validRole(o.DefaultRole) {
		writeError(w, http.StatusBadRequest, "new users can start as pending, viewer or operator")
		return
	}
	if g.Enabled {
		if g.ClientID == "" || g.ClientSecret == "" {
			writeError(w, http.StatusBadRequest, "GitHub needs a client ID and secret")
			return
		}
		if len(g.Orgs) == 0 {
			writeError(w, http.StatusBadRequest, "list at least one GitHub organisation that may sign in")
			return
		}
	}
	if o.Enabled {
		if o.ClientID == "" || o.ClientSecret == "" || !strings.HasPrefix(o.Issuer, "http") {
			writeError(w, http.StatusBadRequest, "OpenID Connect needs an issuer URL, client ID and secret")
			return
		}
		if len(o.Domains) == 0 {
			writeError(w, http.StatusBadRequest, `list the allowed email domains, or * to accept everyone the provider signs in`)
			return
		}
		if _, err := s.oidcDiscover(r.Context(), o.Issuer); err != nil {
			writeError(w, http.StatusBadRequest, "could not reach the provider: "+err.Error())
			return
		}
	}
	if o.Name == "" {
		o.Name = "Single sign-on"
	}
	if req.HidePassword && !g.Enabled && !o.Enabled {
		req.HidePassword = false
	}
	b, _ := json.Marshal(req)
	if err := s.store.SetSetting(r.Context(), settingSSO, string(b)); err != nil {
		s.internal(w, err)
		return
	}
	s.event(r.Context(), "", "info", "settings.sso", "Sign-in methods updated")
	writeJSON(w, http.StatusOK, s.ssoView(r.Context()))
}

type ssoProvider struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// handleSSOProviders tells the sign-in page which buttons to show. It is public.
func (s *Server) handleSSOProviders(w http.ResponseWriter, r *http.Request) {
	c := s.ssoConfig(r.Context())
	out := []ssoProvider{}
	if c.GitHub.Enabled {
		out = append(out, ssoProvider{"github", "GitHub"})
	}
	if c.OIDC.Enabled {
		out = append(out, ssoProvider{"oidc", c.OIDC.Name})
	}
	writeJSON(w, http.StatusOK, map[string]any{"providers": out, "hide_password": c.HidePassword && len(out) > 0})
}

// --- flow state ---

type ssoFlow struct {
	provider, nonce, verifier string
	expires                   time.Time
}

type ssoState struct {
	mu    sync.Mutex
	flows map[string]ssoFlow
	codes map[string]ssoCode
}

type ssoCode struct {
	token   string
	user    *store.User
	expires time.Time
}

func randString(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func (st *ssoState) putFlow(f ssoFlow) string {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.flows == nil {
		st.flows = map[string]ssoFlow{}
	}
	now := time.Now()
	for k, v := range st.flows {
		if now.After(v.expires) {
			delete(st.flows, k)
		}
	}
	id := randString(24)
	f.expires = now.Add(10 * time.Minute)
	st.flows[id] = f
	return id
}

func (st *ssoState) takeFlow(id string) (ssoFlow, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	f, ok := st.flows[id]
	delete(st.flows, id)
	return f, ok && time.Now().Before(f.expires)
}

func (st *ssoState) putCode(c ssoCode) string {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.codes == nil {
		st.codes = map[string]ssoCode{}
	}
	id := randString(24)
	c.expires = time.Now().Add(2 * time.Minute)
	st.codes[id] = c
	return id
}

func (st *ssoState) takeCode(id string) (ssoCode, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	c, ok := st.codes[id]
	delete(st.codes, id)
	return c, ok && time.Now().Before(c.expires)
}

func (s *Server) ssoRedirectURI(r *http.Request, provider string) string {
	scheme := "http"
	if requestIsHTTPS(r) {
		scheme = "https"
	}
	return scheme + "://" + r.Host + "/api/auth/sso/" + provider + "/callback"
}

func ssoFail(w http.ResponseWriter, r *http.Request, msg string) {
	http.Redirect(w, r, "/?sso_error="+url.QueryEscape(msg), http.StatusFound)
}

// handleSSOStart sends the browser to the provider: GET /api/auth/sso/{provider}/start.
func (s *Server) handleSSOStart(w http.ResponseWriter, r *http.Request) {
	provider := r.PathValue("provider")
	c := s.ssoConfig(r.Context())
	flow := ssoFlow{provider: provider, nonce: randString(16), verifier: randString(32)}
	challenge := sha256.Sum256([]byte(flow.verifier))
	q := url.Values{"response_type": {"code"}, "redirect_uri": {s.ssoRedirectURI(r, provider)}}
	var base string
	switch {
	case provider == "github" && c.GitHub.Enabled:
		base = s.ssoGitHubBase() + "/login/oauth/authorize"
		q.Set("client_id", c.GitHub.ClientID)
		q.Set("scope", "read:user read:org")
	case provider == "oidc" && c.OIDC.Enabled:
		d, err := s.oidcDiscover(r.Context(), c.OIDC.Issuer)
		if err != nil {
			ssoFail(w, r, "could not reach the sign-in provider")
			return
		}
		base = d.AuthorizationEndpoint
		q.Set("client_id", c.OIDC.ClientID)
		q.Set("scope", "openid email profile")
		q.Set("nonce", flow.nonce)
		q.Set("code_challenge", base64.RawURLEncoding.EncodeToString(challenge[:]))
		q.Set("code_challenge_method", "S256")
	default:
		ssoFail(w, r, "that sign-in method is not enabled")
		return
	}
	q.Set("state", s.sso.putFlow(flow))
	sep := "?"
	if strings.Contains(base, "?") {
		sep = "&"
	}
	http.Redirect(w, r, base+sep+q.Encode(), http.StatusFound)
}

type ssoIdentity struct {
	external string   // provider:subject
	names    []string // candidate usernames, best first
	allowed  bool     // passed the organisation or domain allow-list
	groups   []string
	grouped  bool // the role comes from group membership
	role     string
}

func (s *Server) handleSSOCallback(w http.ResponseWriter, r *http.Request) {
	provider := r.PathValue("provider")
	flow, ok := s.sso.takeFlow(r.URL.Query().Get("state"))
	if !ok || flow.provider != provider {
		ssoFail(w, r, "the sign-in expired or was already used. Try again")
		return
	}
	if e := r.URL.Query().Get("error"); e != "" {
		ssoFail(w, r, "the provider refused the sign-in: "+e)
		return
	}
	code := r.URL.Query().Get("code")
	c := s.ssoConfig(r.Context())
	var (
		id   *ssoIdentity
		role string
		err  error
	)
	switch provider {
	case "github":
		if !c.GitHub.Enabled {
			ssoFail(w, r, "that sign-in method is not enabled")
			return
		}
		role = c.GitHub.DefaultRole
		id, err = s.githubIdentity(r.Context(), c.GitHub, code, s.ssoRedirectURI(r, provider))
	case "oidc":
		if !c.OIDC.Enabled {
			ssoFail(w, r, "that sign-in method is not enabled")
			return
		}
		role = c.OIDC.DefaultRole
		id, err = s.oidcIdentity(r.Context(), c.OIDC, code, flow, s.ssoRedirectURI(r, provider))
	default:
		ssoFail(w, r, "unknown sign-in method")
		return
	}
	if err != nil {
		s.event(r.Context(), "", "warn", "auth.sso_failed", "Single sign-on failed: "+err.Error())
		ssoFail(w, r, err.Error())
		return
	}
	u, msg := s.ssoUser(r.Context(), id, role)
	if u == nil {
		ssoFail(w, r, msg)
		return
	}
	tok, err := s.newSession(r.Context(), u)
	if err != nil {
		ssoFail(w, r, "internal error")
		return
	}
	s.event(r.Context(), "", "info", "auth.login", u.Username+" signed in with "+provider)
	http.Redirect(w, r, "/?sso="+s.sso.putCode(ssoCode{token: tok, user: u}), http.StatusFound)
}

// handleSSOExchange swaps the one-time code from the redirect for a session token.
func (s *Server) handleSSOExchange(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code string `json:"code"`
	}
	if !decode(w, r, &req) {
		return
	}
	c, ok := s.sso.takeCode(req.Code)
	if !ok {
		writeError(w, http.StatusUnauthorized, "that sign-in link expired. Try again")
		return
	}
	writeJSON(w, http.StatusOK, loginResponse{Token: c.token, identity: &identity{Name: c.user.Username, Role: c.user.Role}})
}

var usernameClean = func(r rune) rune {
	switch {
	case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
		return r
	}
	return -1
}

// ssoUser finds or creates the account for an identity, or returns why it can't sign in.
func (s *Server) ssoUser(ctx context.Context, id *ssoIdentity, defaultRole string) (*store.User, string) {
	if !id.allowed {
		return nil, "your account is not in the organisation or domain allowed to sign in"
	}
	u, err := s.store.UserByExternal(ctx, id.external)
	if errors.Is(err, store.ErrNotFound) {
		// Existing password accounts are never claimed by name: a provider user called "admin"
		// must not take over the local admin. A new account gets a free name instead.
		if u == nil {
			role, disabled := defaultRole, false
			if id.grouped {
				role = id.role
			}
			if role == "pending" {
				role, disabled = store.RoleViewer, true
			}
			for i := 0; i < 20 && u == nil; i++ {
				base := "user"
				if len(id.names) > 0 {
					base = id.names[0]
				}
				if i > 0 {
					base = fmt.Sprintf("%s-%d", base, i+1)
				}
				if !usernameRE.MatchString(base) {
					continue
				}
				if cand, e := s.store.CreateExternalUser(ctx, base, role, id.external, disabled); e == nil {
					u = cand
				} else if !errors.Is(e, store.ErrConflict) {
					return nil, "internal error"
				}
			}
			if u == nil {
				return nil, "could not create an account for you"
			}
		}
	} else if err != nil {
		return nil, "internal error"
	}
	if id.grouped && !u.Disabled && u.Role != id.role {
		u.Role = id.role
		s.store.UpdateUser(ctx, u)
	}
	if u.Disabled {
		return nil, "your account is waiting for an administrator to approve it"
	}
	return u, ""
}

func sanitizeName(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.Map(usernameClean, s)
	for len(s) < 3 && s != "" {
		s += "0"
	}
	if len(s) > 32 {
		s = s[:32]
	}
	return s
}

// --- GitHub ---

func (s *Server) ssoGitHubBase() string {
	if s.ssoGitHubURL != "" {
		return s.ssoGitHubURL
	}
	return "https://github.com"
}

func (s *Server) postForm(ctx context.Context, endpoint string, form url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("the provider returned HTTP %d", resp.StatusCode)
	}
	return json.Unmarshal(body, out)
}

func (s *Server) httpGetJSON(ctx context.Context, endpoint, bearer string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("the provider returned HTTP %d", resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out)
}

func (s *Server) githubIdentity(ctx context.Context, c SSOGitHub, code, redirect string) (*ssoIdentity, error) {
	var tok struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error_description"`
	}
	if err := s.postForm(ctx, s.ssoGitHubBase()+"/login/oauth/access_token", url.Values{"client_id": {c.ClientID}, "client_secret": {c.ClientSecret}, "code": {code}, "redirect_uri": {redirect}}, &tok); err != nil {
		return nil, fmt.Errorf("GitHub sign-in failed: %w", err)
	}
	if tok.AccessToken == "" {
		return nil, fmt.Errorf("GitHub sign-in failed: %s", tok.Error)
	}
	var me struct {
		ID    int64  `json:"id"`
		Login string `json:"login"`
	}
	if err := s.httpGetJSON(ctx, s.githubAPI+"/user", tok.AccessToken, &me); err != nil || me.ID == 0 {
		return nil, errors.New("could not read your GitHub profile")
	}
	var orgs []struct {
		Login string `json:"login"`
	}
	if err := s.httpGetJSON(ctx, s.githubAPI+"/user/orgs?per_page=100", tok.AccessToken, &orgs); err != nil {
		return nil, errors.New("could not read your GitHub organisations")
	}
	id := &ssoIdentity{external: fmt.Sprintf("github:%d", me.ID), names: []string{sanitizeName(me.Login)}}
	for _, o := range orgs {
		for _, want := range c.Orgs {
			if strings.EqualFold(o.Login, want) || want == strings.ToLower(me.Login) {
				id.allowed = true
			}
		}
	}
	return id, nil
}

// --- OpenID Connect ---

type oidcDoc struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
}

func (s *Server) oidcDiscover(ctx context.Context, issuer string) (*oidcDoc, error) {
	var d oidcDoc
	if err := s.httpGetJSON(ctx, strings.TrimRight(issuer, "/")+"/.well-known/openid-configuration", "", &d); err != nil {
		return nil, err
	}
	if d.AuthorizationEndpoint == "" || d.TokenEndpoint == "" {
		return nil, errors.New("that is not an OpenID Connect issuer")
	}
	return &d, nil
}

func (s *Server) oidcIdentity(ctx context.Context, c SSOOIDC, code string, flow ssoFlow, redirect string) (*ssoIdentity, error) {
	d, err := s.oidcDiscover(ctx, c.Issuer)
	if err != nil {
		return nil, errors.New("could not reach the sign-in provider")
	}
	var tok struct {
		IDToken string `json:"id_token"`
		Error   string `json:"error_description"`
	}
	form := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {redirect}, "client_id": {c.ClientID}, "client_secret": {c.ClientSecret}, "code_verifier": {flow.verifier}}
	if err := s.postForm(ctx, d.TokenEndpoint, form, &tok); err != nil {
		return nil, fmt.Errorf("sign-in failed: %w", err)
	}
	// The ID token arrives straight from the token endpoint over TLS, which OpenID Connect
	// Core 3.1.3.7 accepts in place of checking its signature. The claims are still checked.
	parts := strings.Split(tok.IDToken, ".")
	if len(parts) != 3 {
		return nil, errors.New("the provider returned no ID token")
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, errors.New("the ID token is malformed")
	}
	var claims map[string]any
	if json.Unmarshal(raw, &claims) != nil {
		return nil, errors.New("the ID token is malformed")
	}
	str := func(k string) string { v, _ := claims[k].(string); return v }
	if strings.TrimRight(str("iss"), "/") != strings.TrimRight(c.Issuer, "/") {
		return nil, errors.New("the ID token came from a different issuer")
	}
	okAud := false
	switch a := claims["aud"].(type) {
	case string:
		okAud = a == c.ClientID
	case []any:
		for _, x := range a {
			okAud = okAud || x == c.ClientID
		}
	}
	if !okAud {
		return nil, errors.New("the ID token was issued for a different application")
	}
	if exp, _ := claims["exp"].(float64); time.Unix(int64(exp), 0).Before(time.Now()) {
		return nil, errors.New("the ID token has expired")
	}
	if str("nonce") != flow.nonce {
		return nil, errors.New("the ID token does not match this sign-in")
	}
	sub := str("sub")
	if sub == "" {
		return nil, errors.New("the ID token has no subject")
	}
	email := strings.ToLower(str("email"))
	if v, ok := claims["email_verified"].(bool); ok && !v {
		email = ""
	}
	id := &ssoIdentity{external: "oidc:" + sub}
	for _, n := range []string{str("preferred_username"), strings.SplitN(email, "@", 2)[0], str("name")} {
		if n = sanitizeName(n); n != "" {
			id.names = append(id.names, n)
		}
	}
	for _, dom := range c.Domains {
		if dom == "*" || (email != "" && strings.HasSuffix(email, "@"+dom)) {
			id.allowed = true
		}
	}
	if c.AdminGroup != "" || c.OperatorGroup != "" {
		if gs, ok := claims[c.GroupClaim].([]any); ok {
			for _, g := range gs {
				if gstr, ok := g.(string); ok {
					id.groups = append(id.groups, gstr)
				}
			}
		}
		id.grouped, id.role = true, store.RoleViewer
		for _, g := range id.groups {
			if c.OperatorGroup != "" && g == c.OperatorGroup && id.role == store.RoleViewer {
				id.role = store.RoleOperator
			}
			if c.AdminGroup != "" && g == c.AdminGroup {
				id.role = store.RoleAdmin
			}
		}
	}
	return id, nil
}
