package agent

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

func TestCopyDemuxed(t *testing.T) {
	var in bytes.Buffer
	for _, s := range []string{"one\n", "two\n"} {
		hdr := make([]byte, 8)
		hdr[0] = 1
		binary.BigEndian.PutUint32(hdr[4:], uint32(len(s)))
		in.Write(hdr)
		in.WriteString(s)
	}
	var out bytes.Buffer
	if err := copyDemuxed(&out, &in); err != nil || out.String() != "one\ntwo\n" {
		t.Fatalf("%q %v", out.String(), err)
	}
}

// A fake Docker that speaks the exec upgrade: it echoes whatever the shell is sent.
func TestExecShell(t *testing.T) {
	var resized string
	mux := http.NewServeMux()
	mux.HandleFunc("POST /containers/c1/exec", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"Id":"e1"}`)
	})
	mux.HandleFunc("POST /exec/e1/resize", func(w http.ResponseWriter, r *http.Request) {
		resized = r.URL.RawQuery
	})
	mux.HandleFunc("POST /exec/e1/start", func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		conn, buf, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		buf.WriteString("HTTP/1.1 101 UPGRADED\r\nContent-Type: application/vnd.docker.raw-stream\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\nready$ ")
		buf.Flush()
		io.Copy(conn, buf) // echo
	})
	sock := filepath.Join(t.TempDir(), "d.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &httptest.Server{Listener: l, Config: &http.Server{Handler: mux}}
	srv.Start()
	defer srv.Close()

	d, err := NewDocker("unix://" + sock)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sh, err := d.ExecShell(ctx, "c1", 100, 30)
	if err != nil {
		t.Fatal(err)
	}
	defer sh.Close()
	read := func(n int) string {
		b := make([]byte, n)
		if _, err := io.ReadFull(sh, b); err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	if got := read(7); got != "ready$ " {
		t.Fatalf("banner %q", got)
	}
	sh.Write([]byte("ls\n"))
	if got := read(3); got != "ls\n" {
		t.Fatalf("echo %q", got)
	}
	if resized != "h=30&w=100" {
		t.Fatalf("resize %q", resized)
	}
}
