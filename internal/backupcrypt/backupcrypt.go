// Package backupcrypt password-protects backup archives.
//
// File layout: "ORCHBK01", a 4-byte PBKDF2 iteration count, a 16-byte random salt, then
// chunks of up to 64 KiB of plaintext, each sealed with AES-256-GCM and stored as a 4-byte
// ciphertext length followed by the ciphertext. The key comes from the password and the
// file's own salt, so every file has its own key and a counter is a safe nonce. The last
// chunk is flagged in the authenticated data, so a truncated file fails instead of
// decrypting to a shorter archive.
package backupcrypt

import (
	"bufio"
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
)

const (
	magic     = "ORCHBK01"
	chunkSize = 64 << 10
	saltSize  = 16
	headerLen = len(magic) + 4 + saltSize
)

// Iterations is the PBKDF2 cost for new files; tests lower it. Each file records its own.
var Iterations = 600_000

var (
	ErrWrongPassword = errors.New("wrong password, or the backup is damaged")
	ErrCorrupt       = errors.New("the backup file is damaged or incomplete")
	ErrNotEncrypted  = errors.New("not a password-protected backup")
)

func newAEAD(password string, salt []byte, iter int) (cipher.AEAD, error) {
	key, err := pbkdf2.Key(sha256.New, password, salt, iter, 32)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func nonce(counter uint64) []byte {
	n := make([]byte, 12)
	binary.BigEndian.PutUint64(n[4:], counter)
	return n
}

// IsEncrypted reports whether data starts like a protected backup.
func IsEncrypted(prefix []byte) bool { return bytes.HasPrefix(prefix, []byte(magic)) }

type writer struct {
	w       io.Writer
	aead    cipher.AEAD
	buf     []byte
	counter uint64
	err     error
}

// NewWriter returns a writer that encrypts into w. Close must be called to write the final chunk.
func NewWriter(w io.Writer, password string) (io.WriteCloser, error) {
	salt := make([]byte, saltSize)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	aead, err := newAEAD(password, salt, Iterations)
	if err != nil {
		return nil, err
	}
	hdr := make([]byte, 0, headerLen)
	hdr = append(hdr, magic...)
	hdr = binary.BigEndian.AppendUint32(hdr, uint32(Iterations))
	hdr = append(hdr, salt...)
	if _, err := w.Write(hdr); err != nil {
		return nil, err
	}
	return &writer{w: w, aead: aead, buf: make([]byte, 0, chunkSize)}, nil
}

func (e *writer) seal(final bool) error {
	flag := []byte{0}
	if final {
		flag[0] = 1
	}
	ct := e.aead.Seal(nil, nonce(e.counter), e.buf, flag)
	e.counter++
	e.buf = e.buf[:0]
	var l [4]byte
	binary.BigEndian.PutUint32(l[:], uint32(len(ct)))
	if _, err := e.w.Write(l[:]); err != nil {
		return err
	}
	_, err := e.w.Write(ct)
	return err
}

func (e *writer) Write(p []byte) (int, error) {
	if e.err != nil {
		return 0, e.err
	}
	n := len(p)
	for len(p) > 0 {
		take := min(chunkSize-len(e.buf), len(p))
		e.buf = append(e.buf, p[:take]...)
		p = p[take:]
		if len(e.buf) == chunkSize {
			// Keep the last full chunk back until we know whether more follows: it
			// may need the final flag. Close handles it.
			if len(p) == 0 {
				break
			}
			if e.err = e.seal(false); e.err != nil {
				return 0, e.err
			}
		}
	}
	return n, nil
}

func (e *writer) Close() error {
	if e.err != nil {
		return e.err
	}
	return e.seal(true)
}

type reader struct {
	r       *bufio.Reader
	aead    cipher.AEAD
	counter uint64
	plain   []byte
	done    bool
	err     error
}

// NewReader opens a protected backup. It reads the first chunk straight away so a wrong
// password is reported here, before the caller has sent anything to anyone.
func NewReader(r io.Reader, password string) (io.Reader, error) {
	br := bufio.NewReaderSize(r, chunkSize+64)
	hdr := make([]byte, headerLen)
	if _, err := io.ReadFull(br, hdr); err != nil || !IsEncrypted(hdr) {
		return nil, ErrNotEncrypted
	}
	iter := int(binary.BigEndian.Uint32(hdr[len(magic):]))
	if iter < 1000 || iter > 10_000_000 {
		return nil, ErrCorrupt
	}
	aead, err := newAEAD(password, hdr[len(magic)+4:], iter)
	if err != nil {
		return nil, err
	}
	d := &reader{r: br, aead: aead}
	if err := d.next(); err != nil {
		if d.counter == 0 {
			return nil, ErrWrongPassword
		}
		return nil, err
	}
	return d, nil
}

// next decrypts the following chunk into plain.
func (d *reader) next() error {
	var l [4]byte
	if _, err := io.ReadFull(d.r, l[:]); err != nil {
		return ErrCorrupt
	}
	n := binary.BigEndian.Uint32(l[:])
	if n < uint32(d.aead.Overhead()) || n > chunkSize+uint32(d.aead.Overhead()) {
		return ErrCorrupt
	}
	ct := make([]byte, n)
	if _, err := io.ReadFull(d.r, ct); err != nil {
		return ErrCorrupt
	}
	// The chunk is final when nothing follows it.
	_, peek := d.r.Peek(1)
	final := peek == io.EOF
	flag := []byte{0}
	if final {
		flag[0] = 1
	}
	pt, err := d.aead.Open(nil, nonce(d.counter), ct, flag)
	if err != nil {
		if d.counter > 0 {
			return ErrCorrupt
		}
		return ErrWrongPassword
	}
	d.counter++
	d.plain, d.done = pt, final
	return nil
}

func (d *reader) Read(p []byte) (int, error) {
	for len(d.plain) == 0 {
		if d.err != nil {
			return 0, d.err
		}
		if d.done {
			return 0, io.EOF
		}
		if err := d.next(); err != nil {
			d.err = err
			return 0, err
		}
	}
	n := copy(p, d.plain)
	d.plain = d.plain[n:]
	return n, nil
}
