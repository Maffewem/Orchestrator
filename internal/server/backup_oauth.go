package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/Maffewem/Orchestrator/internal/store"
)

// Cloud storage sign-in. The administrator registers their own OAuth app with Google, Dropbox
// or Microsoft and enters its client ID and secret here; connecting a destination then runs the
// authorization-code flow from the dashboard and writes the resulting rclone remote itself, so
// nobody has to run `rclone config`. rclone remains the engine that uploads and refreshes tokens.

const settingBackupOAuth = "backup_oauth_clients"

type oauthClient struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret,omitempty"`
}

type oauthProvider struct {
	ID, Name      string
	AuthURL       string
	TokenURL      string
	Scope         string
	AuthParams    url.Values
	RcloneType    string
	DefaultFolder string
	Console       string // where the app is registered
	Setup         string // what to configure there
}

var oauthProviders = []oauthProvider{
	{
		ID: "google", Name: "Google Drive", RcloneType: "drive", DefaultFolder: "Orchestrator backups",
		AuthURL: "https://accounts.google.com/o/oauth2/v2/auth", TokenURL: "https://oauth2.googleapis.com/token",
		Scope: "https://www.googleapis.com/auth/drive.file", AuthParams: url.Values{"access_type": {"offline"}, "prompt": {"consent"}},
		Console: "https://console.cloud.google.com/apis/credentials",
		Setup:   "Create a project, enable the Google Drive API, configure the OAuth consent screen (add yourself as a test user, or publish it, otherwise the sign-in expires after 7 days), then create an OAuth client of type Web application with the redirect URI below. Only files Orchestrator creates are visible to it (drive.file scope).",
	},
	{
		ID: "dropbox", Name: "Dropbox", RcloneType: "dropbox", DefaultFolder: "Orchestrator backups",
		AuthURL: "https://www.dropbox.com/oauth2/authorize", TokenURL: "https://api.dropboxapi.com/oauth2/token",
		AuthParams: url.Values{"token_access_type": {"offline"}},
		Console:    "https://www.dropbox.com/developers/apps",
		Setup:      "Create an app with Scoped access and the App folder type, add the redirect URI below, and on the Permissions tab enable files.metadata.read, files.metadata.write, files.content.read and files.content.write. Backups go to Apps/<app name>/.",
	},
	{
		ID: "onedrive", Name: "OneDrive", RcloneType: "onedrive", DefaultFolder: "Orchestrator backups",
		AuthURL: "https://login.microsoftonline.com/common/oauth2/v2.0/authorize", TokenURL: "https://login.microsoftonline.com/common/oauth2/v2.0/token",
		Scope:   "Files.ReadWrite offline_access",
		Console: "https://entra.microsoft.com/#view/Microsoft_AAD_RegisteredApps/ApplicationsListBlade",
		Setup:   "Register an application with account type 'Personal Microsoft accounts and work/school accounts', add a Web redirect URI below, create a client secret under Certificates & secrets (copy its Value), and use the Application (client) ID. Delegated permissions Files.ReadWrite and offline_access are requested at sign-in.",
	},
}

func findOAuthProvider(id string) (oauthProvider, bool) {
	for _, p := range oauthProviders {
		if p.ID == id {
			return p, true
		}
	}
	return oauthProvider{}, false
}

type oauthFlow struct {
	provider, destID, redirect string
	expires                    time.Time
}

type oauthState struct {
	mu    sync.Mutex
	flows map[string]oauthFlow
}

func (st *oauthState) put(f oauthFlow) string {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.flows == nil {
		st.flows = map[string]oauthFlow{}
	}
	now := time.Now()
	for k, v := range st.flows {
		if now.After(v.expires) {
			delete(st.flows, k)
		}
	}
	id := randString(24)
	f.expires = now.Add(15 * time.Minute)
	st.flows[id] = f
	return id
}

func (st *oauthState) take(id string) (oauthFlow, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	f, ok := st.flows[id]
	delete(st.flows, id)
	return f, ok && time.Now().Before(f.expires)
}

func (s *Server) oauthClients(ctx context.Context) map[string]oauthClient {
	out := map[string]oauthClient{}
	if v, err := s.store.GetSetting(ctx, settingBackupOAuth); err == nil {
		json.Unmarshal([]byte(v), &out)
	}
	return out
}

// oauthProv returns a provider, with endpoints swapped for tests.
func (s *Server) oauthProv(id string) (oauthProvider, bool) {
	p, ok := findOAuthProvider(id)
	if ok {
		if o, has := s.oauthOverride[id]; has {
			p.AuthURL, p.TokenURL = o[0], o[1]
		}
	}
	return p, ok
}

type oauthProviderView struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	ClientID  string `json:"client_id"`
	SecretSet bool   `json:"secret_set"`
	Console   string `json:"console"`
	Setup     string `json:"setup"`
}

func (s *Server) handleGetBackupOAuth(w http.ResponseWriter, r *http.Request) {
	cl := s.oauthClients(r.Context())
	out := []oauthProviderView{}
	for _, p := range oauthProviders {
		c := cl[p.ID]
		out = append(out, oauthProviderView{ID: p.ID, Name: p.Name, ClientID: c.ClientID, SecretSet: c.ClientSecret != "", Console: p.Console, Setup: p.Setup})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleSetBackupOAuth stores client IDs and secrets: PUT /api/backup-oauth with
// {"google": {"client_id": ..., "client_secret": ...}, ...}. A blank secret keeps the stored one.
func (s *Server) handleSetBackupOAuth(w http.ResponseWriter, r *http.Request) {
	var req map[string]oauthClient
	if !decode(w, r, &req) {
		return
	}
	cur := s.oauthClients(r.Context())
	for id, c := range req {
		if _, ok := findOAuthProvider(id); !ok {
			writeError(w, http.StatusBadRequest, "unknown provider "+id)
			return
		}
		c.ClientID = strings.TrimSpace(c.ClientID)
		if c.ClientSecret == "" {
			c.ClientSecret = cur[id].ClientSecret
		}
		if c.ClientID == "" {
			c = oauthClient{}
		}
		cur[id] = c
	}
	b, _ := json.Marshal(cur)
	if err := s.store.SetSetting(r.Context(), settingBackupOAuth, string(b)); err != nil {
		s.internal(w, err)
		return
	}
	s.handleGetBackupOAuth(w, r)
}

func (s *Server) backupOAuthRedirect(r *http.Request) string {
	scheme := "http"
	if requestIsHTTPS(r) {
		scheme = "https"
	}
	return scheme + "://" + r.Host + "/api/backup-oauth/callback"
}

// handleBackupOAuthStart returns the provider's sign-in URL: POST /api/backup-oauth/{provider}/start
// with an optional {"dest_id": ...} to reconnect an existing destination.
func (s *Server) handleBackupOAuthStart(w http.ResponseWriter, r *http.Request) {
	p, ok := s.oauthProv(r.PathValue("provider"))
	if !ok {
		writeError(w, http.StatusNotFound, "unknown provider")
		return
	}
	var req struct {
		DestID string `json:"dest_id"`
	}
	if r.ContentLength > 0 && !decode(w, r, &req) {
		return
	}
	c := s.oauthClients(r.Context())[p.ID]
	if c.ClientID == "" || c.ClientSecret == "" {
		writeError(w, http.StatusConflict, "enter the "+p.Name+" client ID and secret first")
		return
	}
	redirect := s.backupOAuthRedirect(r)
	state := s.oauth.put(oauthFlow{provider: p.ID, destID: req.DestID, redirect: redirect})
	q := url.Values{"response_type": {"code"}, "client_id": {c.ClientID}, "redirect_uri": {redirect}, "state": {state}}
	if p.Scope != "" {
		q.Set("scope", p.Scope)
	}
	for k, v := range p.AuthParams {
		q[k] = v
	}
	writeJSON(w, http.StatusOK, map[string]string{"url": p.AuthURL + "?" + q.Encode()})
}

func oauthDone(w http.ResponseWriter, r *http.Request, ok bool, msg string) {
	k := "cloud_error"
	if ok {
		k = "cloud"
	}
	http.Redirect(w, r, "/settings?"+k+"="+url.QueryEscape(msg), http.StatusFound)
}

type oauthToken struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
}

// handleBackupOAuthCallback finishes the sign-in: the provider redirects the browser here.
// The one-time state, issued to an authenticated admin, is what authorizes it.
func (s *Server) handleBackupOAuthCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	flow, ok := s.oauth.take(q.Get("state"))
	if !ok {
		oauthDone(w, r, false, "the sign-in expired or was not started here. Try again")
		return
	}
	if e := q.Get("error"); e != "" {
		oauthDone(w, r, false, "the provider refused the sign-in: "+e)
		return
	}
	p, _ := s.oauthProv(flow.provider)
	c := s.oauthClients(r.Context())[p.ID]
	name, err := s.finishBackupOAuth(r.Context(), p, c, flow, q.Get("code"))
	if err != nil {
		oauthDone(w, r, false, err.Error())
		return
	}
	oauthDone(w, r, true, name+" connected")
}

func (s *Server) finishBackupOAuth(ctx context.Context, p oauthProvider, c oauthClient, flow oauthFlow, code string) (string, error) {
	if code == "" {
		return "", errors.New("the provider did not return a code")
	}
	var tok oauthToken
	form := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {flow.redirect}, "client_id": {c.ClientID}, "client_secret": {c.ClientSecret}}
	if err := s.postForm(ctx, p.TokenURL, form, &tok); err != nil {
		return "", fmt.Errorf("could not exchange the sign-in code (check the client ID, secret and redirect URI): %w", err)
	}
	if tok.AccessToken == "" || tok.RefreshToken == "" {
		return "", errors.New("the provider returned no refresh token, so backups could not stay signed in. Remove the app's access in your account and connect again")
	}
	tj, _ := json.Marshal(map[string]string{
		"access_token": tok.AccessToken, "token_type": "Bearer", "refresh_token": tok.RefreshToken,
		"expiry": time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).UTC().Format(time.RFC3339),
	})
	conf := fmt.Sprintf("[%s]\ntype = %s\nclient_id = %s\nclient_secret = %s\ntoken = %s\n", p.ID, p.RcloneType, c.ClientID, c.ClientSecret, tj)
	switch p.ID {
	case "google":
		conf += "scope = drive.file\n"
	case "onedrive":
		var d struct {
			ID        string `json:"id"`
			DriveType string `json:"driveType"`
		}
		if err := s.httpGetJSON(ctx, s.graphDriveURL(), tok.AccessToken, &d); err != nil || d.ID == "" {
			return "", errors.New("signed in, but could not read the OneDrive drive")
		}
		conf += fmt.Sprintf("drive_id = %s\ndrive_type = %s\n", d.ID, d.DriveType)
	}
	if err := validateRcloneConf(conf); err != nil {
		return "", err
	}
	dests := s.backupDests(ctx)
	found := false
	for i := range dests {
		if dests[i].ID == flow.destID && dests[i].Kind == "rclone" {
			dests[i].RcloneConf, dests[i].Remote, dests[i].LastError, found = conf, "", "", true
		}
	}
	if !found {
		dests = append(dests, BackupDest{ID: store.NewToken()[:10], Kind: "rclone", Name: p.Name, Enabled: true, Path: p.DefaultFolder, RcloneConf: conf})
	}
	return p.Name, s.saveBackupDests(ctx, dests)
}

func (s *Server) graphDriveURL() string {
	if s.graphURL != "" {
		return s.graphURL
	}
	return "https://graph.microsoft.com/v1.0/me/drive"
}
