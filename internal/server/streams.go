package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Maffewem/Orchestrator/internal/api"
	"github.com/Maffewem/Orchestrator/internal/store"
)

const (
	streamBuffer     = 2 << 20          // output kept for a browser that is catching up
	streamPollWait   = 20 * time.Second // how long a long-poll waits for news
	streamClaimWait  = 30 * time.Second // the agent must pick a stream up within this
	streamClientIdle = 60 * time.Second // no browser activity for this long ends a stream
	streamShellIdle  = 15 * time.Minute // a shell nobody types in is closed
	maxStreams       = 8                // open streams per device
)

// stream is one open pipe. Output is buffered with an absolute offset so the browser can
// long-poll "everything after N" and never miss or repeat bytes.
type stream struct {
	api.StreamRequest
	deviceID string
	user     string

	mu         sync.Mutex
	out        []byte
	base       int64 // offset of out[0]
	in         []byte
	cols, rows int
	resized    bool
	closed     bool
	errMsg     string
	claimed    bool
	created    time.Time
	lastClient time.Time
	lastInput  time.Time
	wake       chan struct{}

	// Audit trail for shells: what was typed, a line at a time. Input typed at a password
	// prompt is recorded as hidden.
	line   []byte
	esc    bool
	tail   string
	secret bool
}

const maxAuditLine = 512

// auditInput folds typed bytes into command lines and returns each finished one. Callers hold mu.
func (st *stream) auditInput(data []byte) []string {
	var done []string
	for _, b := range data {
		switch {
		case st.esc:
			// Arrow keys and friends: ESC [ A. The sequence ends at the first letter or ~.
			if (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z') || b == '~' {
				st.esc = false
			}
		case b == 0x1b:
			st.esc = true
		case b == '\r' || b == '\n':
			if len(st.line) > 0 {
				if st.secret {
					done = append(done, "[hidden input]")
				} else {
					done = append(done, string(st.line))
				}
			}
			st.line, st.secret = st.line[:0], false
		case b == 0x7f || b == 0x08:
			if n := len(st.line); n > 0 {
				st.line = st.line[:n-1]
			}
		case b == 0x03 || b == 0x15: // Ctrl-C, Ctrl-U: the line is thrown away
			st.line = st.line[:0]
		case b >= 0x20 && len(st.line) < maxAuditLine:
			st.line = append(st.line, b)
		}
	}
	return done
}

// noteOutput watches the end of the output for a password prompt, so what is typed next is
// not written to the audit log. Callers hold mu.
func (st *stream) noteOutput(data []byte) {
	st.tail += strings.ToLower(string(data))
	if len(st.tail) > 80 {
		st.tail = st.tail[len(st.tail)-80:]
	}
	t := strings.TrimRight(st.tail, " \r\n")
	if strings.HasSuffix(t, ":") && (strings.Contains(t, "password") || strings.Contains(t, "passphrase") || strings.Contains(t, "token")) {
		st.secret = true
	}
}

func (st *stream) signal() {
	close(st.wake)
	st.wake = make(chan struct{})
}

// finish ends the stream (idempotent). Callers hold mu.
func (st *stream) finish(msg string) {
	if st.closed {
		return
	}
	st.closed, st.errMsg = true, msg
	st.signal()
}

// expire closes the stream when nobody is using it. Callers hold mu.
func (st *stream) expire(now time.Time) {
	switch {
	case st.closed:
	case !st.claimed && now.Sub(st.created) > streamClaimWait:
		st.finish("the device's agent did not answer. Update it from the Updates page if it is old.")
	case now.Sub(st.lastClient) > streamClientIdle:
		st.finish("")
	case st.Kind == api.StreamShell && now.Sub(st.lastInput) > streamShellIdle:
		st.finish("the shell was closed after 15 minutes without input")
	}
}

type streamHub struct {
	mu      sync.Mutex
	streams map[string]*stream
	wake    chan struct{} // closed when a new stream is waiting for an agent
}

func newStreamHub() *streamHub {
	return &streamHub{streams: map[string]*stream{}, wake: make(chan struct{})}
}

func (h *streamHub) get(id string) *stream {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.streams[id]
}

// open registers a stream, dropping dead ones first. It fails when the device is at its limit.
func (h *streamHub) open(req api.StreamRequest, deviceID, user string) (*stream, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := time.Now()
	live := 0
	for id, st := range h.streams {
		st.mu.Lock()
		st.expire(now)
		dead := st.closed && now.Sub(st.lastClient) > 2*time.Minute
		if !st.closed && st.deviceID == deviceID {
			live++
		}
		st.mu.Unlock()
		if dead {
			delete(h.streams, id)
		}
	}
	if live >= maxStreams {
		return nil, fmt.Errorf("too many open streams on this device (limit %d); close one first", maxStreams)
	}
	st := &stream{StreamRequest: req, deviceID: deviceID, user: user, created: now, lastClient: now, lastInput: now,
		wake: make(chan struct{}), cols: req.Cols, rows: req.Rows}
	h.streams[req.ID] = st
	close(h.wake)
	h.wake = make(chan struct{})
	return st, nil
}

// claim returns the device's streams the agent has not seen yet, waiting for one if needed.
func (h *streamHub) claim(ctx context.Context, deviceID string) []api.StreamRequest {
	timer := time.NewTimer(streamPollWait)
	defer timer.Stop()
	for {
		h.mu.Lock()
		var out []api.StreamRequest
		for _, st := range h.streams {
			st.mu.Lock()
			if st.deviceID == deviceID && !st.claimed && !st.closed {
				st.claimed = true
				out = append(out, st.StreamRequest)
			}
			st.mu.Unlock()
		}
		wake := h.wake
		h.mu.Unlock()
		if len(out) > 0 {
			return out
		}
		select {
		case <-wake:
		case <-timer.C:
			return nil
		case <-ctx.Done():
			return nil
		}
	}
}

// --- browser side ---

// streamFor loads a stream for a signed-in user. A shell needs an administrator.
func (s *Server) streamFor(w http.ResponseWriter, r *http.Request) (*stream, bool) {
	st := s.streams.get(r.PathValue("id"))
	if st == nil {
		writeError(w, http.StatusNotFound, "no such stream (it may have ended)")
		return nil, false
	}
	if st.Kind == api.StreamShell {
		if id := identityFrom(r); id == nil || store.RoleRank(id.Role) < store.RoleRank(store.RoleAdmin) {
			writeError(w, http.StatusForbidden, "only administrators can use a container shell")
			return nil, false
		}
	}
	return st, true
}

// handleOpenStream starts live logs (operator) or a shell (administrator) on a container.
func (s *Server) handleOpenStream(w http.ResponseWriter, r *http.Request) {
	d, ok := s.lookupDevice(w, r)
	if !ok {
		return
	}
	var req struct {
		Kind      string `json:"kind"`
		Container string `json:"container"`
		Tail      int    `json:"tail"`
		Cols      int    `json:"cols"`
		Rows      int    `json:"rows"`
	}
	if !decode(w, r, &req) {
		return
	}
	id := identityFrom(r)
	switch req.Kind {
	case api.StreamLogs:
	case api.StreamShell:
		if id == nil || store.RoleRank(id.Role) < store.RoleRank(store.RoleAdmin) {
			writeError(w, http.StatusForbidden, "only administrators can open a container shell")
			return
		}
	default:
		writeError(w, http.StatusBadRequest, "kind must be logs or shell")
		return
	}
	if !hasCap(d, api.CapStream) {
		writeError(w, http.StatusConflict, errAgentTooOld(d, "live logs and shells").Error())
		return
	}
	var target *api.Container
	for i := range d.Containers {
		if c := &d.Containers[i]; c.ID == req.Container || c.Name == req.Container {
			target = c
		}
	}
	if target == nil {
		writeError(w, http.StatusNotFound, "that device has no such container")
		return
	}
	if req.Kind == api.StreamShell && target.State != "running" {
		writeError(w, http.StatusConflict, target.Name+" is not running, so there is nothing to open a shell in")
		return
	}
	if req.Tail <= 0 || req.Tail > 5000 {
		req.Tail = 200
	}
	if req.Cols < 20 || req.Cols > 500 {
		req.Cols = 80
	}
	if req.Rows < 5 || req.Rows > 200 {
		req.Rows = 24
	}
	who := "token"
	if id != nil {
		who = id.Name
	}
	st, err := s.streams.open(api.StreamRequest{ID: newStreamID(), Kind: req.Kind, Container: target.ID, Tail: req.Tail, Cols: req.Cols, Rows: req.Rows}, d.ID, who)
	if err != nil {
		writeError(w, http.StatusTooManyRequests, err.Error())
		return
	}
	if req.Kind == api.StreamShell {
		s.event(r.Context(), d.ID, "warning", "shell.opened", fmt.Sprintf("%s opened a shell in %s on %s", who, target.Name, d.Name))
	}
	writeJSON(w, http.StatusCreated, map[string]string{"id": st.ID})
}

// handleStreamOutput long-polls for output after ?from=N.
func (s *Server) handleStreamOutput(w http.ResponseWriter, r *http.Request) {
	st, ok := s.streamFor(w, r)
	if !ok {
		return
	}
	from, _ := strconv.ParseInt(r.URL.Query().Get("from"), 10, 64)
	timer := time.NewTimer(streamPollWait)
	defer timer.Stop()
	for {
		st.mu.Lock()
		st.lastClient = time.Now()
		st.expire(st.lastClient)
		end := st.base + int64(len(st.out))
		if from < st.base {
			from = st.base
		}
		if from < end || st.closed {
			resp := api.StreamOutput{Data: append([]byte(nil), st.out[from-st.base:]...), Closed: st.closed, Error: st.errMsg}
			st.mu.Unlock()
			writeJSON(w, http.StatusOK, map[string]any{"data": resp.Data, "next": end, "closed": resp.Closed, "error": resp.Error})
			return
		}
		wake := st.wake
		st.mu.Unlock()
		select {
		case <-wake:
		case <-timer.C:
			writeJSON(w, http.StatusOK, map[string]any{"data": []byte{}, "next": end})
			return
		case <-r.Context().Done():
			return
		}
	}
}

// handleStreamInput passes keystrokes (and a new window size) to a shell.
func (s *Server) handleStreamInput(w http.ResponseWriter, r *http.Request) {
	st, ok := s.streamFor(w, r)
	if !ok {
		return
	}
	if st.Kind != api.StreamShell {
		writeError(w, http.StatusBadRequest, "only shells take input")
		return
	}
	var in api.StreamInput
	if !decode(w, r, &in) {
		return
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.closed {
		writeError(w, http.StatusGone, "the shell has ended")
		return
	}
	now := time.Now()
	st.lastClient = now
	if len(in.Data) > 0 {
		st.lastInput = now
		if len(st.in)+len(in.Data) > 1<<20 {
			writeError(w, http.StatusRequestEntityTooLarge, "too much input at once")
			return
		}
		st.in = append(st.in, in.Data...)
		for _, l := range st.auditInput(in.Data) {
			s.event(r.Context(), st.deviceID, "info", "shell.input", fmt.Sprintf("%s ran in %s: %s", st.user, st.Container, l))
		}
	}
	if in.Cols > 0 && in.Rows > 0 && (in.Cols != st.cols || in.Rows != st.rows) {
		st.cols, st.rows, st.resized = in.Cols, in.Rows, true
	}
	st.signal()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleCloseStream(w http.ResponseWriter, r *http.Request) {
	st, ok := s.streamFor(w, r)
	if !ok {
		return
	}
	st.mu.Lock()
	st.finish("")
	st.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

// --- agent side ---

// handleAgentStreamPoll hands the agent the streams it should open.
func (s *Server) handleAgentStreamPoll(w http.ResponseWriter, r *http.Request) {
	d := deviceFrom(r)
	writeJSON(w, http.StatusOK, api.StreamPoll{Streams: s.streams.claim(r.Context(), d.ID)})
}

func (s *Server) agentStream(w http.ResponseWriter, r *http.Request) (*stream, bool) {
	st := s.streams.get(r.PathValue("id"))
	if st == nil || st.deviceID != deviceFrom(r).ID {
		writeError(w, http.StatusNotFound, "no such stream")
		return nil, false
	}
	return st, true
}

// handleAgentStreamOutput appends the container's output. The reply says whether to go on.
func (s *Server) handleAgentStreamOutput(w http.ResponseWriter, r *http.Request) {
	st, ok := s.agentStream(w, r)
	if !ok {
		return
	}
	var out api.StreamOutput
	if !decode(w, r, &out) {
		return
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	st.expire(time.Now())
	if !st.closed {
		st.out = append(st.out, out.Data...)
		if st.Kind == api.StreamShell {
			st.noteOutput(out.Data)
		}
		if over := len(st.out) - streamBuffer; over > 0 {
			st.out = st.out[over:]
			st.base += int64(over)
		}
		if out.Closed {
			st.finish(out.Error)
		}
		st.signal()
	}
	writeJSON(w, http.StatusOK, map[string]bool{"closed": st.closed})
}

// handleAgentStreamInput long-polls for keystrokes and resizes to deliver to the container.
func (s *Server) handleAgentStreamInput(w http.ResponseWriter, r *http.Request) {
	st, ok := s.agentStream(w, r)
	if !ok {
		return
	}
	timer := time.NewTimer(streamPollWait)
	defer timer.Stop()
	for {
		st.mu.Lock()
		st.expire(time.Now())
		if len(st.in) > 0 || st.resized || st.closed {
			in := api.StreamInput{Data: st.in, Closed: st.closed}
			if st.resized {
				in.Cols, in.Rows = st.cols, st.rows
			}
			st.in, st.resized = nil, false
			st.mu.Unlock()
			writeJSON(w, http.StatusOK, in)
			return
		}
		wake := st.wake
		st.mu.Unlock()
		select {
		case <-wake:
		case <-timer.C:
			writeJSON(w, http.StatusOK, api.StreamInput{})
			return
		case <-r.Context().Done():
			return
		}
	}
}

func newStreamID() string {
	b := make([]byte, 12)
	rand.Read(b)
	return "str_" + hex.EncodeToString(b)
}
