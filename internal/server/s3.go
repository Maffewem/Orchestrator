package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"
)

// A minimal S3 client (path-style, Signature V4) for the two calls backups need: PUT and
// DELETE of one object. It works with AWS S3, Cloudflare R2, Backblaze B2 and MinIO.

type s3Target struct {
	Endpoint, Region, Bucket, AccessKey, SecretKey string
}

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}

func sha256Hex(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

// signatureV4 returns the hex signature for a request whose headers are all signed.
func signatureV4(method, uri, query string, headers map[string]string, payloadHash, secret, region, amzDate string) string {
	names := make([]string, 0, len(headers))
	lower := map[string]string{}
	for k, v := range headers {
		l := strings.ToLower(k)
		names = append(names, l)
		lower[l] = strings.TrimSpace(v)
	}
	sort.Strings(names)
	var canon strings.Builder
	for _, n := range names {
		canon.WriteString(n + ":" + lower[n] + "\n")
	}
	creq := strings.Join([]string{method, uri, query, canon.String(), strings.Join(names, ";"), payloadHash}, "\n")
	day := amzDate[:8]
	scope := day + "/" + region + "/s3/aws4_request"
	sts := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + sha256Hex([]byte(creq))
	k := hmacSHA256([]byte("AWS4"+secret), day)
	k = hmacSHA256(k, region)
	k = hmacSHA256(k, "s3")
	k = hmacSHA256(k, "aws4_request")
	return hex.EncodeToString(hmacSHA256(k, sts))
}

func (t s3Target) objectURL(key string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimRight(t.Endpoint, "/"))
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, fmt.Errorf("endpoint must be a full URL such as https://s3.amazonaws.com")
	}
	u.Path = "/" + t.Bucket + "/" + key
	return u, nil
}

func (t s3Target) do(ctx context.Context, method, key string, body io.Reader, size int64, payloadHash string) error {
	u, err := t.objectURL(key)
	if err != nil {
		return err
	}
	now := time.Now().UTC().Format("20060102T150405Z")
	hdr := map[string]string{"host": u.Host, "x-amz-content-sha256": payloadHash, "x-amz-date": now}
	sig := signatureV4(method, u.EscapedPath(), "", hdr, payloadHash, t.SecretKey, t.Region, now)
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return err
	}
	req.ContentLength = size
	req.Header.Set("x-amz-content-sha256", payloadHash)
	req.Header.Set("x-amz-date", now)
	req.Header.Set("Authorization", fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s/%s/s3/aws4_request, SignedHeaders=host;x-amz-content-sha256;x-amz-date, Signature=%s", t.AccessKey, now[:8], t.Region, sig))
	resp, err := (&http.Client{Timeout: 2 * time.Hour}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("S3 returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return nil
}

// PutFile uploads a local file as key.
func (t s3Target) PutFile(ctx context.Context, key, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	size, err := io.Copy(h, f)
	if err != nil {
		return err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	return t.do(ctx, http.MethodPut, key, f, size, hex.EncodeToString(h.Sum(nil)))
}

func (t s3Target) Delete(ctx context.Context, key string) error {
	return t.do(ctx, http.MethodDelete, key, nil, 0, sha256Hex(nil))
}
