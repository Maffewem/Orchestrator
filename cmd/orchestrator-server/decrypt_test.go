package main

import (
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
