package agent

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestBackupMountsSkipsReadOnlyAndSockets(t *testing.T) {
	raw := json.RawMessage(`{"Name":"/web","Mounts":[
		{"Type":"volume","Name":"data","Destination":"/data","RW":true},
		{"Type":"bind","Source":"/srv/conf","Destination":"/conf","RW":false},
		{"Type":"bind","Source":"/var/run/docker.sock","Destination":"/var/run/docker.sock","RW":true},
		{"Type":"bind","Source":"/srv/www","Destination":"/www","RW":true},
		{"Type":"tmpfs","Destination":"/tmp","RW":true}]}`)
	mounts, name, err := backupMounts(raw)
	if err != nil || name != "web" || len(mounts) != 2 {
		t.Fatalf("%v %q %+v", err, name, mounts)
	}
	if mounts[0].Destination != "/data" || mounts[1].Destination != "/www" || mounts[1].Index != 1 {
		t.Fatalf("%+v", mounts)
	}
}

func TestCopyPrefixed(t *testing.T) {
	var src bytes.Buffer
	w := tar.NewWriter(&src)
	w.WriteHeader(&tar.Header{Name: "data/", Typeflag: tar.TypeDir, Mode: 0o755})
	w.WriteHeader(&tar.Header{Name: "data/a.txt", Mode: 0o644, Size: 2})
	w.Write([]byte("hi"))
	w.Close()

	var out bytes.Buffer
	ow := tar.NewWriter(&out)
	if err := copyPrefixed(ow, &src, "mounts/0/"); err != nil {
		t.Fatal(err)
	}
	ow.Close()
	r := tar.NewReader(&out)
	var names []string
	for {
		h, err := r.Next()
		if err != nil {
			break
		}
		names = append(names, h.Name)
	}
	if strings.Join(names, ",") != "mounts/0/data/,mounts/0/data/a.txt" {
		t.Fatalf("%v", names)
	}
}

// TestBackupRestoreRoundTrip runs against a real Docker engine and is skipped without one.
func TestBackupRestoreRoundTrip(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not installed")
	}
	if err := exec.Command("docker", "image", "inspect", "alpine:3.20").Run(); err != nil {
		t.Skip("alpine:3.20 not available locally")
	}
	sh := func(args ...string) string {
		out, err := exec.Command("docker", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("docker %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	name := "orch-backup-test"
	exec.Command("docker", "rm", "-f", name).Run()
	exec.Command("docker", "volume", "rm", "-f", name+"-vol").Run()
	t.Cleanup(func() {
		exec.Command("docker", "rm", "-f", name).Run()
		exec.Command("docker", "volume", "rm", "-f", name+"-vol").Run()
	})
	id := sh("run", "-d", "--name", name, "-v", name+"-vol:/data", "alpine:3.20", "sh", "-c",
		"mkdir -p /data/sub && echo original > /data/sub/file.txt && sleep 300")
	time.Sleep(time.Second)

	var archive []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		if r.Method == http.MethodPost {
			archive, _ = io.ReadAll(r.Body)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Write(archive)
	}))
	defer srv.Close()

	host := os.Getenv("DOCKER_HOST")
	if host == "" {
		host = "unix:///var/run/docker.sock"
	}
	a, err := New(Config{Server: srv.URL, DockerHost: host})
	if err != nil {
		t.Fatal(err)
	}
	a.state.AgentToken = "tok"
	ctx := context.Background()

	if err := a.backup(ctx, id, "bkp_1"); err != nil {
		t.Fatal(err)
	}
	if len(archive) == 0 {
		t.Fatal("nothing uploaded")
	}
	sh("exec", name, "sh", "-c", "echo changed > /data/sub/file.txt && echo extra > /data/new.txt")
	if err := a.restoreBackup(ctx, id, "bkp_1"); err != nil {
		t.Fatal(err)
	}
	if got := sh("exec", name, "cat", "/data/sub/file.txt"); got != "original" {
		t.Fatalf("file after restore = %q", got)
	}
	if got := sh("inspect", "-f", "{{.State.Running}}", name); got != "true" {
		t.Fatalf("container running = %s", got)
	}
}
