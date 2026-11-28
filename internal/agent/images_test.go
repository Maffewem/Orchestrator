package agent

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallFirstBoot(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "etc"), 0o755)
	if err := installFirstBoot(root, "http://192.168.0.184:8080", "join-token-123456", "pi-new"); err != nil {
		t.Fatal(err)
	}
	env, _ := os.ReadFile(filepath.Join(root, "etc/orchestrator-firstboot/env"))
	if !strings.Contains(string(env), "ORCH_TOKEN=join-token-123456") || !strings.Contains(string(env), "ORCH_NAME=pi-new") {
		t.Errorf("env: %s", env)
	}
	if fi, _ := os.Stat(filepath.Join(root, "etc/orchestrator-firstboot/env")); fi.Mode().Perm() != 0o600 {
		t.Errorf("env must be root-only, got %v", fi.Mode().Perm())
	}
	if l, err := os.Readlink(filepath.Join(root, "etc/systemd/system/multi-user.target.wants/orchestrator-firstboot.service")); err != nil || !strings.HasSuffix(l, "orchestrator-firstboot.service") {
		t.Errorf("unit not enabled: %v %q", err, l)
	}
	// Idempotent.
	if err := installFirstBoot(root, "http://192.168.0.184:8080", "join-token-123456", ""); err != nil {
		t.Fatal(err)
	}
	// Nothing that could break out of the shell env file or the unit is accepted.
	bad := []struct{ server, token, name string }{
		{"http://x;reboot", "join-token-123456", ""},
		{"ftp://x", "join-token-123456", ""},
		{"http://x", "tok en!", ""},
		{"http://x", "short", ""},
		{"http://x", "join-token-123456", "a b; rm -rf /"},
	}
	for _, b := range bad {
		if err := installFirstBoot(root, b.server, b.token, b.name); err == nil {
			t.Errorf("accepted %+v", b)
		}
	}
	if err := installFirstBoot(t.TempDir(), "http://x", "join-token-123456", ""); err == nil {
		t.Error("accepted a directory that is not a root filesystem")
	}
}

func TestBootOrder(t *testing.T) {
	cfg := "[all]\nBOOT_UART=1\nBOOT_ORDER=0xF41\nPOWER_OFF_ON_HALT=0\n"
	if got := parseBootOrder(cfg); got != "0xf41" {
		t.Errorf("parse: %q", got)
	}
	if got := withBootOrder(cfg, "0xf14"); !strings.Contains(got, "BOOT_ORDER=0xf14\n") || strings.Contains(got, "F41") || !strings.Contains(got, "BOOT_UART=1") {
		t.Errorf("replace: %q", got)
	}
	if got := withBootOrder("BOOT_UART=1", "0xf12"); got != "BOOT_UART=1\nBOOT_ORDER=0xf12\n" {
		t.Errorf("append: %q", got)
	}
	for _, ok := range []string{"0xf41", "0xf146", "0x1"} {
		if !bootOrderValid.MatchString(ok) {
			t.Errorf("rejected %s", ok)
		}
	}
	for _, bad := range []string{"f41", "0x", "0xf4g", "0x123456789", "0x0", "0x3"} {
		if bootOrderValid.MatchString(bad) {
			t.Errorf("accepted %s", bad)
		}
	}
}

type countWriter struct{ bytes.Buffer }

func TestCopyLimited(t *testing.T) {
	var w countWriter
	if n, err := copyLimited(&w, strings.NewReader("hello"), 5); err != nil || n != 5 {
		t.Errorf("fits: %d %v", n, err)
	}
	w.Reset()
	if _, err := copyLimited(&w, strings.NewReader("hello!"), 5); err == nil || !strings.Contains(err.Error(), "larger than the disk") {
		t.Errorf("too large: %v", err)
	}
}
