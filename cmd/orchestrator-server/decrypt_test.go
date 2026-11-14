package main

import (
	"github.com/Maffewem/Orchestrator/internal/store"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/Maffewem/Orchestrator/internal/backupcrypt"
)

func TestDecryptBackup(t *testing.T) {
	backupcrypt.Iterations = 1000
	dir := t.TempDir()
	enc, out := filepath.Join(dir, "b.tar.gz.enc"), filepath.Join(dir, "b.tar.gz")
	f, _ := os.Create(enc)
	w, _ := backupcrypt.NewWriter(f, "pass-phrase-1")
	w.Write([]byte("archive"))
	w.Close()
	f.Close()

	if err := decryptBackup(enc, out, "wrong"); err == nil {
		t.Fatal("wrong password accepted")
	}
	if _, err := os.Stat(out); err == nil {
		t.Fatal("output left behind after a failed decrypt")
	}
	if err := decryptBackup(enc, out, "pass-phrase-1"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(out); string(b) != "archive" {
		t.Fatalf("got %q", b)
	}
}

func TestImportBackupStagesDatabase(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.db")
	st, err := store.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	st.SetSetting(t.Context(), "base_domain", "example.com")
	plain := filepath.Join(dir, "plain.db")
	if err := st.Snapshot(t.Context(), plain, true); err != nil {
		t.Fatal(err)
	}
	st.Close()
	enc := filepath.Join(dir, "bundle.db.enc")
	in, _ := os.Open(plain)
	out, _ := os.Create(enc)
	w, _ := backupcrypt.NewWriter(out, "pw-12345")
	io.Copy(w, in)
	w.Close()
	out.Close()

	target := filepath.Join(dir, "data", "orchestrator.db")
	os.MkdirAll(filepath.Dir(target), 0o755)
	if err := importBackup(enc, target, "wrong"); err == nil {
		t.Fatal("wrong password accepted")
	}
	if err := importBackup(enc, target, "pw-12345"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(store.PendingRestorePath(target)); err != nil {
		t.Fatal("nothing staged")
	}
	if ok, err := store.ApplyPendingRestore(target); !ok || err != nil {
		t.Fatalf("apply: %v %v", ok, err)
	}
	// Opening it on a server with no key adopts the one the bundle carries.
	st2, err := store.Open(target)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	if v, _ := st2.GetSetting(t.Context(), "base_domain"); v != "example.com" {
		t.Fatalf("setting = %q", v)
	}
	if entries, _ := filepath.Glob(target + ".restore*"); len(entries) != 0 {
		t.Fatalf("temporary files left: %v", entries)
	}
}
