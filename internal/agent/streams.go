package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Maffewem/Orchestrator/internal/api"
)

// Live logs and shells. The agent only ever dials out, so a stream is three long-lived HTTP
// calls the agent makes: one to hear about new streams, one to push output, one to pull input.

// LogsFollow streams a container's output from the last tail lines onwards. tty says whether
// the stream is raw (true) or Docker's 8-byte-framed stdout/stderr multiplex (false).
func (d *Docker) LogsFollow(ctx context.Context, id string, tail int) (rc io.ReadCloser, tty bool, err error) {
	_, tty, err = d.Inspect(ctx, id)
	if err != nil {
		return nil, false, err
	}
	q := url.Values{"stdout": {"1"}, "stderr": {"1"}, "timestamps": {"1"}, "follow": {"1"}, "tail": {strconv.Itoa(tail)}}
	resp, err := d.request(ctx, http.MethodGet, "/containers/"+id+"/logs", q, nil)
	if err != nil {
		return nil, false, err
	}
	return resp.Body, tty, nil
}

// execSession is an interactive process inside a container with a terminal attached.
type execSession struct {
	d    *Docker
	id   string
	conn interface {
		io.Writer
		io.Closer
	}
	r io.Reader
}

func (e *execSession) Read(p []byte) (int, error)  { return e.r.Read(p) }
func (e *execSession) Write(p []byte) (int, error) { return e.conn.Write(p) }
func (e *execSession) Close() error                { return e.conn.Close() }

func (e *execSession) Resize(ctx context.Context, cols, rows int) error {
	q := url.Values{"w": {strconv.Itoa(cols)}, "h": {strconv.Itoa(rows)}}
	return e.d.call(ctx, http.MethodPost, "/exec/"+e.id+"/resize", q, nil, nil)
}

// ExecShell starts bash (or sh) in a container with a TTY and returns the attached session.
func (d *Docker) ExecShell(ctx context.Context, container string, cols, rows int) (*execSession, error) {
	var created struct {
		ID string `json:"Id"`
	}
	err := d.call(ctx, http.MethodPost, "/containers/"+container+"/exec", nil, map[string]any{
		"AttachStdin": true, "AttachStdout": true, "AttachStderr": true, "Tty": true,
		"Env": []string{"TERM=xterm-256color"},
		"Cmd": []string{"/bin/sh", "-c", "command -v bash >/dev/null 2>&1 && exec bash || exec sh"},
	}, &created)
	if err != nil {
		return nil, err
	}
	if d.dial == nil {
		return nil, errors.New("shells are not supported for this Docker host")
	}
	conn, err := d.dial(ctx)
	if err != nil {
		return nil, err
	}
	body := []byte(`{"Detach":false,"Tty":true}`)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://docker/exec/"+created.ID+"/start", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "tcp")
	if err := req.Write(conn); err != nil {
		conn.Close()
		return nil, err
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		conn.Close()
		return nil, err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols && resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		conn.Close()
		return nil, fmt.Errorf("docker: %s", strings.TrimSpace(string(msg)))
	}
	sess := &execSession{d: d, id: created.ID, conn: conn, r: br}
	sess.Resize(ctx, cols, rows)
	return sess, nil
}

// get fetches JSON from the control plane without the short client timeout, for long-polls.
func (a *Agent) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.cfg.Server+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+a.state.AgentToken)
	req.Header.Set("User-Agent", "orchestrator-agent/"+Version)
	resp, err := (&http.Client{Transport: a.http.Transport}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return errUnauthorized
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s: %s", path, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// streamLoop asks the control plane for streams to open and serves each in its own goroutine.
func (a *Agent) streamLoop(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		pctx, cancel := context.WithTimeout(ctx, 40*time.Second)
		var poll api.StreamPoll
		err := a.get(pctx, "/api/agent/streams", &poll)
		cancel()
		if err != nil {
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, time.Minute)
			continue
		}
		backoff = time.Second
		for _, req := range poll.Streams {
			go a.serveStream(ctx, req)
		}
	}
}

// pushOutput sends output to the control plane and reports whether the stream goes on.
func (a *Agent) pushOutput(ctx context.Context, id string, out api.StreamOutput) bool {
	var res struct {
		Closed bool `json:"closed"`
	}
	if err := a.post(ctx, "/api/agent/streams/"+id+"/output", a.state.AgentToken, out, &res); err != nil {
		return false
	}
	return !res.Closed
}

func (a *Agent) serveStream(ctx context.Context, req api.StreamRequest) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var err error
	switch req.Kind {
	case api.StreamLogs:
		err = a.serveLogs(ctx, req)
	case api.StreamShell:
		err = a.serveShell(ctx, req)
	default:
		err = fmt.Errorf("unknown stream kind %q", req.Kind)
	}
	out := api.StreamOutput{Closed: true}
	if err != nil && ctx.Err() == nil {
		out.Error = err.Error()
	}
	a.pushOutput(context.WithoutCancel(ctx), req.ID, out)
}

// batcher collects bytes and flushes them every few milliseconds so a chatty container
// costs a handful of requests a second, not one per line.
type batcher struct {
	mu  sync.Mutex
	buf []byte
}

func (b *batcher) Write(p []byte) (int, error) {
	b.mu.Lock()
	b.buf = append(b.buf, p...)
	b.mu.Unlock()
	return len(p), nil
}

func (b *batcher) take() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := b.buf
	b.buf = nil
	return out
}

// flushLoop pushes batched output until done closes, then once more. A closed stream
// cancels the work through cancel.
func (a *Agent) flushLoop(ctx context.Context, id string, b *batcher, done <-chan struct{}, cancel context.CancelFunc) {
	t := time.NewTicker(120 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-done:
			if data := b.take(); len(data) > 0 {
				a.pushOutput(context.WithoutCancel(ctx), id, api.StreamOutput{Data: data})
			}
			return
		case <-t.C:
			if data := b.take(); len(data) > 0 {
				if !a.pushOutput(ctx, id, api.StreamOutput{Data: data}) {
					cancel()
					return
				}
			}
		}
	}
}

func (a *Agent) serveLogs(ctx context.Context, req api.StreamRequest) error {
	rc, tty, err := a.docker.LogsFollow(ctx, req.Container, req.Tail)
	if err != nil {
		return err
	}
	defer rc.Close()
	go func() { <-ctx.Done(); rc.Close() }()

	b := &batcher{}
	done := make(chan struct{})
	flushed := make(chan struct{})
	ctx2, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() { a.flushLoop(ctx2, req.ID, b, done, cancel); close(flushed) }()
	go func() { <-ctx2.Done(); rc.Close() }()

	if tty {
		_, err = io.Copy(b, rc)
	} else {
		err = copyDemuxed(b, rc)
	}
	close(done)
	<-flushed
	if ctx2.Err() != nil {
		return nil
	}
	return err
}

// copyDemuxed writes the payload of Docker's framed stdout/stderr stream to w.
func copyDemuxed(w io.Writer, r io.Reader) error {
	hdr := make([]byte, 8)
	for {
		if _, err := io.ReadFull(r, hdr); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return nil
			}
			return err
		}
		if _, err := io.CopyN(w, r, int64(binary.BigEndian.Uint32(hdr[4:]))); err != nil {
			return nil
		}
	}
}

func (a *Agent) serveShell(ctx context.Context, req api.StreamRequest) error {
	sess, err := a.docker.ExecShell(ctx, req.Container, req.Cols, req.Rows)
	if err != nil {
		return err
	}
	defer sess.Close()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() { <-ctx.Done(); sess.Close() }()

	b := &batcher{}
	done := make(chan struct{})
	flushed := make(chan struct{})
	go func() { a.flushLoop(ctx, req.ID, b, done, cancel); close(flushed) }()

	// Keystrokes: long-poll the control plane and write whatever arrives to the shell.
	go func() {
		defer cancel()
		for ctx.Err() == nil {
			pctx, pcancel := context.WithTimeout(ctx, 40*time.Second)
			var in api.StreamInput
			err := a.get(pctx, "/api/agent/streams/"+req.ID+"/input", &in)
			pcancel()
			if err != nil {
				if ctx.Err() == nil {
					time.Sleep(time.Second)
				}
				continue
			}
			if in.Cols > 0 && in.Rows > 0 {
				sess.Resize(ctx, in.Cols, in.Rows)
			}
			if len(in.Data) > 0 {
				if _, err := sess.Write(in.Data); err != nil {
					return
				}
			}
			if in.Closed {
				return
			}
		}
	}()

	_, err = io.Copy(b, sess) // returns when the shell exits or the stream closes
	close(done)
	<-flushed
	if ctx.Err() != nil {
		return nil
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}
