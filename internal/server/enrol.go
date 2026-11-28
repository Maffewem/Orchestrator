package server

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Maffewem/Orchestrator/internal/store"
)

// Enrolment tokens are scoped alternatives to the shared join token: each one expires, has a use
// limit and can be revoked, and records which devices registered with it. They are meant for
// anything that leaves a credential on removable media or on a disk (rebuild first-boot jobs,
// master-image bundles, USB bootstrap), where the shared join token would be too much to leak.

const (
	settingEnrolTokens = "enrol_tokens"
	enrolPrefix        = "enr_"
	maxEnrolTTL        = 30 * 24 * time.Hour
)

type enrolToken struct {
	ID        string    `json:"id"`
	Label     string    `json:"label"`
	Hash      string    `json:"hash"`
	Plain     string    `json:"plain,omitempty"` // only for tokens a rebuild hands to one device; cleared once delivered
	CommandID string    `json:"command_id,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	CreatedBy string    `json:"created_by,omitempty"`
	ExpiresAt time.Time `json:"expires_at"`
	MaxUses   int       `json:"max_uses"`
	Uses      int       `json:"uses"`
	Revoked   bool      `json:"revoked,omitempty"`
	Devices   []string  `json:"devices,omitempty"`
}

// EnrolTokenView is what the API returns: never the secret or its hash.
type EnrolTokenView struct {
	ID        string    `json:"id"`
	Label     string    `json:"label"`
	CreatedAt time.Time `json:"created_at"`
	CreatedBy string    `json:"created_by,omitempty"`
	ExpiresAt time.Time `json:"expires_at"`
	MaxUses   int       `json:"max_uses"`
	Uses      int       `json:"uses"`
	Status    string    `json:"status"` // active, expired, used up or revoked
	Devices   []string  `json:"devices,omitempty"`
	Token     string    `json:"token,omitempty"` // only in the response that creates it
}

var enrolMu sync.Mutex

func hashEnrol(tok string) string {
	h := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(h[:])
}

func (t *enrolToken) status(now time.Time) string {
	switch {
	case t.Revoked:
		return "revoked"
	case !now.Before(t.ExpiresAt):
		return "expired"
	case t.Uses >= t.MaxUses:
		return "used up"
	}
	return "active"
}

func (s *Server) loadEnrol(r *http.Request) ([]enrolToken, error) {
	raw, err := s.store.GetSetting(r.Context(), settingEnrolTokens)
	if errors.Is(err, store.ErrNotFound) || (err == nil && raw == "") {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var list []enrolToken
	return list, json.Unmarshal([]byte(raw), &list)
}

func (s *Server) saveEnrol(r *http.Request, list []enrolToken) error {
	// Tokens that ended more than a week ago are dropped; the audit log keeps the history.
	keep := list[:0]
	for _, t := range list {
		if t.status(time.Now()) == "active" || time.Since(t.ExpiresAt) < 7*24*time.Hour {
			keep = append(keep, t)
		}
	}
	b, _ := json.Marshal(keep)
	return s.store.SetSetting(r.Context(), settingEnrolTokens, string(b))
}

// mintEnrol creates a token. The caller holds enrolMu.
func (s *Server) mintEnrol(r *http.Request, label string, ttl time.Duration, uses int, who, commandID string, keepPlain bool) (string, *enrolToken, error) {
	list, err := s.loadEnrol(r)
	if err != nil {
		return "", nil, err
	}
	var b [24]byte
	rand.Read(b[:])
	var idb [6]byte
	rand.Read(idb[:])
	tok := enrolPrefix + hex.EncodeToString(b[:])
	t := enrolToken{ID: "et_" + hex.EncodeToString(idb[:]), Label: label, Hash: hashEnrol(tok), CommandID: commandID,
		CreatedAt: time.Now().UTC(), CreatedBy: who, ExpiresAt: time.Now().UTC().Add(ttl), MaxUses: uses}
	if keepPlain {
		t.Plain = tok
	}
	list = append(list, t)
	return tok, &t, s.saveEnrol(r, list)
}

// useEnrol checks tok and reserves one use. It returns the token id, or "" when tok is not valid.
func (s *Server) useEnrol(r *http.Request, tok string) string {
	if !strings.HasPrefix(tok, enrolPrefix) {
		return ""
	}
	enrolMu.Lock()
	defer enrolMu.Unlock()
	list, err := s.loadEnrol(r)
	if err != nil {
		return ""
	}
	h := hashEnrol(tok)
	for i := range list {
		if list[i].Hash == h && list[i].status(time.Now()) == "active" {
			list[i].Uses++
			if s.saveEnrol(r, list) != nil {
				return ""
			}
			return list[i].ID
		}
	}
	return ""
}

// releaseEnrol gives a reserved use back when registration failed afterwards.
func (s *Server) releaseEnrol(r *http.Request, id string) {
	enrolMu.Lock()
	defer enrolMu.Unlock()
	list, _ := s.loadEnrol(r)
	for i := range list {
		if list[i].ID == id && list[i].Uses > 0 {
			list[i].Uses--
		}
	}
	s.saveEnrol(r, list)
}

func (s *Server) noteEnrolDevice(r *http.Request, id, device string) {
	enrolMu.Lock()
	defer enrolMu.Unlock()
	list, _ := s.loadEnrol(r)
	for i := range list {
		if list[i].ID == id {
			list[i].Devices = append(list[i].Devices, device)
		}
	}
	s.saveEnrol(r, list)
}

// takePlain returns the secret of the token minted for commandID and forgets it, so it is handed over once.
func (s *Server) takePlain(r *http.Request, commandID string) string {
	enrolMu.Lock()
	defer enrolMu.Unlock()
	list, _ := s.loadEnrol(r)
	for i := range list {
		if list[i].CommandID == commandID && list[i].Plain != "" && list[i].status(time.Now()) == "active" {
			p := list[i].Plain
			list[i].Plain = ""
			s.saveEnrol(r, list)
			return p
		}
	}
	return ""
}

func (s *Server) handleListEnrolTokens(w http.ResponseWriter, r *http.Request) {
	enrolMu.Lock()
	list, err := s.loadEnrol(r)
	enrolMu.Unlock()
	if err != nil {
		s.internal(w, err)
		return
	}
	out := []EnrolTokenView{}
	for _, t := range list {
		out = append(out, EnrolTokenView{ID: t.ID, Label: t.Label, CreatedAt: t.CreatedAt, CreatedBy: t.CreatedBy, ExpiresAt: t.ExpiresAt,
			MaxUses: t.MaxUses, Uses: t.Uses, Status: t.status(time.Now()), Devices: t.Devices})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleCreateEnrolToken(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Label    string `json:"label"`
		TTLHours int    `json:"ttl_hours"`
		MaxUses  int    `json:"max_uses"`
	}
	if !decode(w, r, &req) {
		return
	}
	req.Label = strings.TrimSpace(req.Label)
	if req.Label == "" || len(req.Label) > 80 {
		writeError(w, http.StatusBadRequest, "give the token a label (up to 80 characters), such as where its card or disk is kept")
		return
	}
	if req.TTLHours <= 0 {
		req.TTLHours = 24
	}
	ttl := time.Duration(req.TTLHours) * time.Hour
	if ttl > maxEnrolTTL {
		writeError(w, http.StatusBadRequest, "a token can last at most 30 days")
		return
	}
	if req.MaxUses <= 0 {
		req.MaxUses = 1
	}
	if req.MaxUses > 100 {
		writeError(w, http.StatusBadRequest, "a token can enrol at most 100 devices")
		return
	}
	who := "an admin"
	if i := identityFrom(r); i != nil {
		who = i.Name
	}
	enrolMu.Lock()
	tok, t, err := s.mintEnrol(r, req.Label, ttl, req.MaxUses, who, "", false)
	enrolMu.Unlock()
	if err != nil {
		s.internal(w, err)
		return
	}
	s.event(r.Context(), "", "warning", "enrol.create", fmt.Sprintf("%s: created enrolment token %q (%d use(s), expires %s)", who, req.Label, req.MaxUses, t.ExpiresAt.Format("2006-01-02 15:04 MST")))
	writeJSON(w, http.StatusCreated, EnrolTokenView{ID: t.ID, Label: t.Label, CreatedAt: t.CreatedAt, CreatedBy: who, ExpiresAt: t.ExpiresAt,
		MaxUses: t.MaxUses, Status: "active", Token: tok})
}

func (s *Server) handleRevokeEnrolToken(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	enrolMu.Lock()
	list, err := s.loadEnrol(r)
	found := false
	label := ""
	for i := range list {
		if list[i].ID == id {
			list[i].Revoked, list[i].Plain, found, label = true, "", true, list[i].Label
		}
	}
	if err == nil && found {
		err = s.saveEnrol(r, list)
	}
	enrolMu.Unlock()
	switch {
	case err != nil:
		s.internal(w, err)
	case !found:
		writeError(w, http.StatusNotFound, "no such token")
	default:
		who := "an admin"
		if i := identityFrom(r); i != nil {
			who = i.Name
		}
		s.event(r.Context(), "", "warning", "enrol.revoke", fmt.Sprintf("%s: revoked enrolment token %q", who, label))
		w.WriteHeader(http.StatusNoContent)
	}
}
