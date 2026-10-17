package agent

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func frame(stream byte, s string) []byte {
	h := make([]byte, 8)
	h[0] = stream
	binary.BigEndian.PutUint32(h[4:], uint32(len(s)))
	return append(h, s...)
}

func TestDemux(t *testing.T) {
	var b bytes.Buffer
	b.Write(frame(1, "hello\n"))
	b.Write(frame(2, "oops\n"))
	got, err := demux(&b)
	if err != nil || got != "hello\noops\n" {
		t.Fatalf("demux = %q, %v", got, err)
	}
}

func TestSplitImage(t *testing.T) {
	cases := map[string][2]string{
		"nginx":                     {"nginx", "latest"},
		"nginx:1.27":                {"nginx", "1.27"},
		"ghcr.io/o/app:v1":          {"ghcr.io/o/app", "v1"},
		"registry.local:5000/app":   {"registry.local:5000/app", "latest"},
		"registry.local:5000/app:2": {"registry.local:5000/app", "2"},
		"app@sha256:abc":            {"app@sha256:abc", ""},
	}
	for in, want := range cases {
		repo, tag := splitImage(in)
		if repo != want[0] || tag != want[1] {
			t.Errorf("splitImage(%q) = %q, %q", in, repo, tag)
		}
	}
}

func TestRedact(t *testing.T) {
	in := "failed to clone https://x-access-token:ghp_abc@github.com/o/r.git"
	if got := redact(in); got != "failed to clone https://***@github.com/o/r.git" {
		t.Fatalf("redact = %q", got)
	}
}
