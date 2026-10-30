package server

import (
	"net/http"
	"testing"
)

func TestBranchRe(t *testing.T) {
	for _, ok := range []string{"main", "feature/x", "v1.2", "_wip"} {
		if !branchRe.MatchString(ok) {
			t.Errorf("%q should be accepted", ok)
		}
	}
	for _, bad := range []string{"--upload-pack=x", "-x", "a b", "a#b", "a:b", ""} {
		if branchRe.MatchString(bad) {
			t.Errorf("%q should be refused", bad)
		}
	}
}

func TestClientIPTrustedProxies(t *testing.T) {
	trusted, err := parseTrustedProxies("10.0.0.0/8, 127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{trusted: trusted}
	req := func(remote, xff string) *http.Request {
		r, _ := http.NewRequest("GET", "/", nil)
		r.RemoteAddr = remote
		if xff != "" {
			r.Header.Set("X-Forwarded-For", xff)
		}
		return r
	}
	if got := s.clientIP(req("203.0.113.9:1", "1.2.3.4")); got != "203.0.113.9" {
		t.Errorf("untrusted peer must not choose its address, got %s", got)
	}
	if got := s.clientIP(req("10.1.1.1:1", "198.51.100.7")); got != "198.51.100.7" {
		t.Errorf("trusted proxy should pass the client through, got %s", got)
	}
	if got := s.clientIP(req("127.0.0.1:1", "6.6.6.6, 198.51.100.7, 10.2.2.2")); got != "198.51.100.7" {
		t.Errorf("spoofed leading hops must be ignored, got %s", got)
	}
	if _, err := parseTrustedProxies("nonsense"); err == nil {
		t.Error("a bad entry should be an error")
	}
}

func TestShellAudit(t *testing.T) {
	st := &stream{}
	got := st.auditInput([]byte("ls -la\r"))
	if len(got) != 1 || got[0] != "ls -la" {
		t.Fatalf("got %q", got)
	}
	st.auditInput([]byte("ecx\x7fho hi\x1b[A"))
	if got = st.auditInput([]byte("\r")); len(got) != 1 || got[0] != "echo hi" {
		t.Fatalf("editing keys: got %q", got)
	}
	st.noteOutput([]byte("[sudo] password for pi: "))
	st.auditInput([]byte("hunter2"))
	if got = st.auditInput([]byte("\n")); len(got) != 1 || got[0] != "[hidden input]" {
		t.Fatalf("password must be hidden, got %q", got)
	}
}
