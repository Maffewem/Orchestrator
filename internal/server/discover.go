package server

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/Maffewem/Orchestrator/internal/api"
	"github.com/Maffewem/Orchestrator/internal/store"
)

// Agents connect outwards to one address. When that address changes (the server moved from
// Wi-Fi to ethernet, got a new DHCP lease) they would be stranded, so the server helps them
// in two ways: it hands every agent the list of addresses the admin has set (so a planned
// move can be announced ahead of time), and it can prove its identity to an agent that goes
// looking for it on the local network.
const (
	settingIdentityKey = "server_identity_key"
	settingAgentURLs   = "agent_server_urls"
)

// loadIdentity reads the server's Ed25519 identity key, creating it on first run.
func (s *Server) loadIdentity(ctx context.Context) error {
	v, err := s.store.GetSetting(ctx, settingIdentityKey)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	if seed, derr := hex.DecodeString(v); derr == nil && len(seed) == ed25519.SeedSize {
		s.identity = ed25519.NewKeyFromSeed(seed)
		return nil
	}
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		return err
	}
	s.identity = priv
	return s.store.SetSetting(ctx, settingIdentityKey, hex.EncodeToString(priv.Seed()))
}

func (s *Server) serverKey() string {
	if s.identity == nil {
		return ""
	}
	return base64.StdEncoding.EncodeToString(s.identity.Public().(ed25519.PublicKey))
}

// handleIdentify signs the caller's nonce. It is open on purpose: it reveals only the public
// key, and an agent that has lost its server has no working address to authenticate against.
func (s *Server) handleIdentify(w http.ResponseWriter, r *http.Request) {
	nonce := r.URL.Query().Get("nonce")
	if b, err := hex.DecodeString(nonce); err != nil || len(b) < 16 || len(b) > 64 {
		writeError(w, http.StatusBadRequest, "nonce must be 16 to 64 random bytes in hex")
		return
	}
	writeJSON(w, http.StatusOK, api.IdentifyResponse{
		ServerKey: s.serverKey(),
		Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(s.identity, api.IdentifyMessage(nonce))),
	})
}

// agentURLs are the addresses agents should know this server by, in order of preference.
func (s *Server) agentURLs(ctx context.Context) []string {
	v, _ := s.store.GetSetting(ctx, settingAgentURLs)
	var out []string
	for _, u := range strings.Split(v, "\n") {
		if u != "" {
			out = append(out, u)
		}
	}
	return out
}

func (s *Server) handleSetAgentURLs(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URLs []string `json:"urls"`
	}
	if !decode(w, r, &req) {
		return
	}
	var clean []string
	seen := map[string]bool{}
	for _, raw := range req.URLs {
		raw = strings.TrimRight(strings.TrimSpace(raw), "/")
		if raw == "" || seen[raw] {
			continue
		}
		u, err := url.Parse(raw)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Path != "" || u.RawQuery != "" || u.User != nil {
			writeError(w, http.StatusBadRequest, "enter addresses such as http://192.168.0.10:8080 or https://orchestrator.example.com: "+raw)
			return
		}
		seen[raw] = true
		clean = append(clean, raw)
	}
	if len(clean) > 8 {
		writeError(w, http.StatusBadRequest, "at most 8 addresses")
		return
	}
	if err := s.store.SetSetting(r.Context(), settingAgentURLs, strings.Join(clean, "\n")); err != nil {
		s.internal(w, err)
		return
	}
	s.event(r.Context(), "", "info", "settings.agent_urls", "Server addresses for agents were changed")
	if clean == nil {
		clean = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"agent_urls": clean})
}
