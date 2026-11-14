package server

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Maffewem/Orchestrator/internal/store"
)

// keyForbidden are the routes an API key can never call, whatever its role, so that a leaked
// key cannot mint more keys, add a webhook that reads every event, or take over accounts.
func keyForbidden(pattern string) bool {
	_, path, _ := strings.Cut(pattern, " ")
	for _, p := range []string{"/api/api-keys", "/api/webhooks", "/api/users", "/api/auth/password"} {
		if strings.HasPrefix(path, p) {
			return true
		}
	}
	return false
}

// --- API keys (admin, signed-in people only) ---

func (s *Server) handleListAPIKeys(w http.ResponseWriter, r *http.Request) {
	ks, err := s.store.ListAPIKeys(r.Context())
	if err != nil {
		s.internal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ks)
}

func (s *Server) handleCreateAPIKey(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name       string `json:"name"`
		Role       string `json:"role"`
		ExpiryDays int    `json:"expiry_days"` // 0 = never
	}
	if !decode(w, r, &req) {
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" || len(req.Name) > 60 {
		writeError(w, http.StatusBadRequest, "give the key a name of up to 60 characters")
		return
	}
	if store.RoleRank(req.Role) == 0 {
		writeError(w, http.StatusBadRequest, "role must be viewer, operator or admin")
		return
	}
	if req.ExpiryDays < 0 || req.ExpiryDays > 3650 {
		writeError(w, http.StatusBadRequest, "expiry must be between 0 (never) and 3650 days")
		return
	}
	var exp time.Time
	if req.ExpiryDays > 0 {
		exp = time.Now().Add(time.Duration(req.ExpiryDays) * 24 * time.Hour)
	}
	k, plain, err := s.store.CreateAPIKey(r.Context(), req.Name, req.Role, identityFrom(r).Name, exp)
	if err != nil {
		s.internal(w, err)
		return
	}
	s.event(r.Context(), "", "info", "apikey.created", fmt.Sprintf("%s created API key %q (%s)", identityFrom(r).Name, k.Name, k.Role))
	writeJSON(w, http.StatusCreated, map[string]any{"key": k, "token": plain})
}

func (s *Server) handleDeleteAPIKey(w http.ResponseWriter, r *http.Request) {
	k, err := s.store.GetAPIKey(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no such key")
		return
	} else if err != nil {
		s.internal(w, err)
		return
	}
	if err := s.store.DeleteAPIKey(r.Context(), k.ID); err != nil {
		s.internal(w, err)
		return
	}
	s.event(r.Context(), "", "warning", "apikey.revoked", fmt.Sprintf("%s revoked API key %q", identityFrom(r).Name, k.Name))
	w.WriteHeader(http.StatusNoContent)
}

// --- webhooks ---

// webhookView is a webhook without its secret, which is shown once when it is created.
type webhookView struct {
	*store.Webhook
	SecretHint string `json:"secret_hint"`
}

func view(w *store.Webhook) webhookView {
	h := ""
	if len(w.Secret) > 4 {
		h = "…" + w.Secret[len(w.Secret)-4:]
	}
	return webhookView{Webhook: w, SecretHint: h}
}

func validWebhookURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return errors.New("the address must be an http:// or https:// URL")
	}
	return nil
}

func cleanEvents(in []string) ([]string, error) {
	var out []string
	for _, e := range in {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		if len(e) > 60 || strings.ContainsAny(e, ", \t") {
			return nil, fmt.Errorf("%q is not an event name", e)
		}
		out = append(out, e)
	}
	if len(out) == 0 {
		return nil, errors.New("choose at least one event (or * for all of them)")
	}
	return out, nil
}

func newWebhookSecret() string {
	b := make([]byte, 24)
	rand.Read(b)
	return "whsec_" + hex.EncodeToString(b)
}

func (s *Server) handleListWebhooks(w http.ResponseWriter, r *http.Request) {
	hs, err := s.store.ListWebhooks(r.Context())
	if err != nil {
		s.internal(w, err)
		return
	}
	out := make([]webhookView, 0, len(hs))
	for _, h := range hs {
		out = append(out, view(h))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleCreateWebhook(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name   string   `json:"name"`
		URL    string   `json:"url"`
		Events []string `json:"events"`
	}
	if !decode(w, r, &req) {
		return
	}
	req.Name, req.URL = strings.TrimSpace(req.Name), strings.TrimSpace(req.URL)
	if req.Name == "" || len(req.Name) > 60 {
		writeError(w, http.StatusBadRequest, "give the webhook a name of up to 60 characters")
		return
	}
	if err := validWebhookURL(req.URL); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	ev, err := cleanEvents(req.Events)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	h, err := s.store.CreateWebhook(r.Context(), req.Name, req.URL, newWebhookSecret(), ev)
	if err != nil {
		s.internal(w, err)
		return
	}
	s.event(r.Context(), "", "info", "webhook.created", fmt.Sprintf("%s added webhook %q", identityFrom(r).Name, h.Name))
	writeJSON(w, http.StatusCreated, map[string]any{"webhook": view(h), "secret": h.Secret})
}

func (s *Server) handleUpdateWebhook(w http.ResponseWriter, r *http.Request) {
	h, err := s.store.GetWebhook(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no such webhook")
		return
	} else if err != nil {
		s.internal(w, err)
		return
	}
	var req struct {
		Name         *string  `json:"name"`
		URL          *string  `json:"url"`
		Events       []string `json:"events"`
		Enabled      *bool    `json:"enabled"`
		RotateSecret bool     `json:"rotate_secret"`
	}
	if !decode(w, r, &req) {
		return
	}
	if req.Name != nil {
		h.Name = strings.TrimSpace(*req.Name)
		if h.Name == "" || len(h.Name) > 60 {
			writeError(w, http.StatusBadRequest, "give the webhook a name of up to 60 characters")
			return
		}
	}
	if req.URL != nil {
		h.URL = strings.TrimSpace(*req.URL)
		if err := validWebhookURL(h.URL); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	if req.Events != nil {
		if h.Events, err = cleanEvents(req.Events); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	if req.Enabled != nil {
		h.Enabled = *req.Enabled
	}
	out := map[string]any{}
	if req.RotateSecret {
		h.Secret = newWebhookSecret()
		out["secret"] = h.Secret
	}
	if err := s.store.UpdateWebhook(r.Context(), h); err != nil {
		s.internal(w, err)
		return
	}
	out["webhook"] = view(h)
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleDeleteWebhook(w http.ResponseWriter, r *http.Request) {
	h, err := s.store.GetWebhook(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no such webhook")
		return
	} else if err != nil {
		s.internal(w, err)
		return
	}
	if err := s.store.DeleteWebhook(r.Context(), h.ID); err != nil {
		s.internal(w, err)
		return
	}
	s.event(r.Context(), "", "warning", "webhook.deleted", fmt.Sprintf("%s deleted webhook %q", identityFrom(r).Name, h.Name))
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleListDeliveries(w http.ResponseWriter, r *http.Request) {
	if _, err := s.store.GetWebhook(r.Context(), r.PathValue("id")); err != nil {
		writeError(w, http.StatusNotFound, "no such webhook")
		return
	}
	ds, err := s.store.ListDeliveries(r.Context(), r.PathValue("id"))
	if err != nil {
		s.internal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ds)
}

// handleTestWebhook sends a "ping" event and waits for the result.
func (s *Server) handleTestWebhook(w http.ResponseWriter, r *http.Request) {
	h, err := s.store.GetWebhook(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no such webhook")
		return
	} else if err != nil {
		s.internal(w, err)
		return
	}
	d := s.deliver(r.Context(), h, hookEvent{Type: "ping", Level: "info", Message: "Test event from Orchestrator", At: time.Now().UTC()}, false)
	writeJSON(w, http.StatusOK, d)
}

// --- delivery ---

// hookEvent is the JSON body a webhook receives.
type hookEvent struct {
	ID       string    `json:"id"`
	Type     string    `json:"type"`
	Level    string    `json:"level"`
	Message  string    `json:"message"`
	DeviceID string    `json:"device_id,omitempty"`
	At       time.Time `json:"created_at"`
}

// hookRetryDelays is the wait before each retry; tests shorten it.
var hookRetryDelays = []time.Duration{2 * time.Second, 10 * time.Second}

// eventMatches reports whether a webhook's patterns select an event kind.
func eventMatches(patterns []string, kind string) bool {
	for _, p := range patterns {
		switch {
		case p == "*" || p == kind:
			return true
		case strings.HasSuffix(p, ".*") && strings.HasPrefix(kind, strings.TrimSuffix(p, "*")):
			return true
		}
	}
	return false
}

// sign computes the signature header value over timestamp + "." + body.
func hookSign(secret, ts string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(ts + "."))
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

// dispatch sends an activity event to every enabled webhook that wants it, in the background.
func (s *Server) dispatch(deviceID, level, kind, msg string) {
	if strings.HasPrefix(kind, "webhook.") {
		return
	}
	ctx := context.Background()
	hs, err := s.store.ListWebhooks(ctx)
	if err != nil || len(hs) == 0 {
		return
	}
	ev := hookEvent{Type: kind, Level: level, Message: msg, DeviceID: deviceID, At: time.Now().UTC()}
	for _, h := range hs {
		if h.Enabled && eventMatches(h.Events, kind) {
			go s.deliver(ctx, h, ev, true)
		}
	}
}

var hookClient = &http.Client{
	Timeout:       10 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// deliver posts one event, retrying failures, and records the outcome.
func (s *Server) deliver(ctx context.Context, h *store.Webhook, ev hookEvent, retry bool) *store.Delivery {
	b := make([]byte, 8)
	rand.Read(b)
	ev.ID = "evt_" + hex.EncodeToString(b)
	body, _ := json.Marshal(ev)
	d := &store.Delivery{WebhookID: h.ID, Event: ev.Type, Payload: string(body)}
	start := time.Now()
	for attempt := 0; ; attempt++ {
		d.Attempts = attempt + 1
		d.HTTPStatus, d.Error = 0, ""
		ts := strconv.FormatInt(time.Now().Unix(), 10)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.URL, bytes.NewReader(body))
		if err != nil {
			d.Error = err.Error()
			break
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", "Orchestrator-Webhook/1")
		req.Header.Set("X-Orchestrator-Event", ev.Type)
		req.Header.Set("X-Orchestrator-Delivery", ev.ID)
		req.Header.Set("X-Orchestrator-Timestamp", ts)
		req.Header.Set("X-Orchestrator-Signature", hookSign(h.Secret, ts, body))
		resp, err := hookClient.Do(req)
		if err != nil {
			d.Error = err.Error()
		} else {
			io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
			resp.Body.Close()
			d.HTTPStatus = resp.StatusCode
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				d.OK = true
				break
			}
			d.Error = "the receiver answered " + resp.Status
		}
		if !retry || attempt >= len(hookRetryDelays) || (d.HTTPStatus >= 400 && d.HTTPStatus < 500 && d.HTTPStatus != 429) {
			break
		}
		time.Sleep(hookRetryDelays[attempt])
	}
	d.DurationMS = time.Since(start).Milliseconds()
	if err := s.store.AddDelivery(ctx, d); err != nil {
		s.log.Error("record webhook delivery", "err", err)
	}
	d.CreatedAt = time.Now().UTC()
	return d
}

// --- OpenAPI ---

type routeInfo struct{ Pattern, Role string }

func (s *Server) handleOpenAPI(w http.ResponseWriter, r *http.Request) {
	paths := map[string]map[string]any{}
	sorted := append([]routeInfo(nil), s.routes...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Pattern < sorted[j].Pattern })
	for _, ri := range sorted {
		method, path, _ := strings.Cut(ri.Pattern, " ")
		var params []map[string]any
		for _, seg := range strings.Split(path, "/") {
			if strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "}") {
				params = append(params, map[string]any{"name": strings.Trim(seg, "{}"), "in": "path", "required": true, "schema": map[string]string{"type": "string"}})
			}
		}
		tag := "misc"
		if parts := strings.Split(strings.TrimPrefix(path, "/api/"), "/"); parts[0] != "" {
			tag = parts[0]
		}
		op := map[string]any{
			"tags":             []string{tag},
			"summary":          method + " " + path,
			"x-required-role":  ri.Role,
			"x-api-key-access": !keyForbidden(ri.Pattern),
			"responses":        map[string]any{"200": map[string]string{"description": "Success"}, "401": map[string]string{"description": "Missing or invalid token"}, "403": map[string]string{"description": "The role is too low"}},
		}
		if len(params) > 0 {
			op["parameters"] = params
		}
		if paths[path] == nil {
			paths[path] = map[string]any{}
		}
		paths[path][strings.ToLower(method)] = op
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"openapi": "3.0.3",
		"info": map[string]any{
			"title":       "Orchestrator API",
			"version":     s.cfg.Version,
			"description": "Send `Authorization: Bearer <API key or session token>`. Each route needs at least the role in `x-required-role`. API keys cannot call routes with `x-api-key-access: false`. Webhooks are signed: `X-Orchestrator-Signature` is `sha256=` + hex HMAC-SHA256 of `<X-Orchestrator-Timestamp>.<body>` with the webhook secret.",
		},
		"servers":    []map[string]string{{"url": "/"}},
		"security":   []map[string][]string{{"bearerAuth": {}}},
		"components": map[string]any{"securitySchemes": map[string]any{"bearerAuth": map[string]string{"type": "http", "scheme": "bearer"}}},
		"paths":      paths,
	})
}
