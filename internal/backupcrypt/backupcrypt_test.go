package backupcrypt

import (
	"bytes"
	"crypto/rand"
	"errors"
	"io"
	"testing"
)

func init() { Iterations = 1000 }

func seal(t *testing.T, data []byte, pw string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := NewWriter(&buf, pw)
	if err != nil {
		t.Fatal(err)
	}
	// Write in odd-sized pieces to exercise chunking.
	for rest := data; len(rest) > 0; {
		n := min(len(rest), 40_000)
		if _, err := w.Write(rest[:n]); err != nil {
			t.Fatal(err)
		}
		rest = rest[n:]
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestRoundTrip(t *testing.T) {
	for _, size := range []int{0, 1, chunkSize - 1, chunkSize, chunkSize + 1, 3*chunkSize + 5} {
		data := make([]byte, size)
		rand.Read(data)
		enc := seal(t, data, "hunter2hunter2")
		// short plaintexts (1 byte) can appear in the ciphertext by chance
		if size >= 16 && bytes.Contains(enc, data[:min(size, 32)]) {
			t.Fatalf("size %d: plaintext visible", size)
		}
		r, err := NewReader(bytes.NewReader(enc), "hunter2hunter2")
		if err != nil {
			t.Fatalf("size %d: %v", size, err)
		}
		got, err := io.ReadAll(r)
		if err != nil || !bytes.Equal(got, data) {
			t.Fatalf("size %d: err=%v equal=%v", size, err, bytes.Equal(got, data))
		}
	}
}

func TestWrongPasswordAndTampering(t *testing.T) {
	data := make([]byte, 3*chunkSize)
	rand.Read(data)
	enc := seal(t, data, "right-password")

	if _, err := NewReader(bytes.NewReader(enc), "wrong-password"); !errors.Is(err, ErrWrongPassword) {
		t.Fatalf("wrong password: %v", err)
	}
	if _, err := NewReader(bytes.NewReader([]byte("not a backup at all, just text")), "x"); !errors.Is(err, ErrNotEncrypted) {
		t.Fatalf("plain input: %v", err)
	}

	// Cutting off the end must fail, not yield a shorter archive.
	cut := enc[:len(enc)-chunkSize]
	r, err := NewReader(bytes.NewReader(cut), "right-password")
	if err == nil {
		_, err = io.ReadAll(r)
	}
	if err == nil {
		t.Fatal("truncated backup decrypted without error")
	}
	// So must a flipped bit in the middle.
	bad := append([]byte(nil), enc...)
	bad[len(bad)/2] ^= 1
	r, err = NewReader(bytes.NewReader(bad), "right-password")
	if err == nil {
		_, err = io.ReadAll(r)
	}
	if err == nil {
		t.Fatal("tampered backup decrypted without error")
	}
}
