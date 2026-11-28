package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// storeServerBackup downloads a server backup into dir, then keeps only the newest keep.
func (a *Agent) storeServerBackup(ctx context.Context, name, dir, keep string) (string, error) {
	if name == "" || name != filepath.Base(name) || !strings.HasPrefix(name, "orchestrator-") {
		return "", fmt.Errorf("invalid backup name %q", name)
	}
	if !filepath.IsAbs(dir) {
		return "", fmt.Errorf("folder %q is not an absolute path", dir)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.server()+"/api/agent/server-backups/"+url.PathEscape(name), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+a.state.AgentToken)
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if err := a.fence(resp); err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("could not download the backup (HTTP %d)", resp.StatusCode)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	dst := filepath.Join(dir, name)
	f, err := os.OpenFile(dst+".part", os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return "", err
	}
	_, err = io.Copy(f, resp.Body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(dst + ".part")
		return "", err
	}
	if err := os.Rename(dst+".part", dst); err != nil {
		return "", err
	}
	if n, err := strconv.Atoi(keep); err == nil && n > 0 {
		entries, _ := os.ReadDir(dir)
		var names []string
		for _, e := range entries {
			if x := e.Name(); strings.HasPrefix(x, "orchestrator-") && (strings.HasSuffix(x, ".db") || strings.HasSuffix(x, ".db.enc")) {
				names = append(names, x)
			}
		}
		sort.Sort(sort.Reverse(sort.StringSlice(names)))
		for i := n; i < len(names); i++ {
			os.Remove(filepath.Join(dir, names[i]))
		}
	}
	return hashFile(dst)
}

// hashFile returns the SHA-256 of a file as hex.
func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// fetchServerBackup reads a copy this device holds. mode "hash" only returns its checksum;
// mode "send" also uploads it to the server (used to restore from this device's copy).
func (a *Agent) fetchServerBackup(ctx context.Context, name, dir, mode string) (string, error) {
	if name == "" || name != filepath.Base(name) || !strings.HasPrefix(name, "orchestrator-") || !filepath.IsAbs(dir) {
		return "", fmt.Errorf("invalid backup name or folder")
	}
	p := filepath.Join(dir, name)
	sum, err := hashFile(p)
	if err != nil {
		return "", fmt.Errorf("the copy is missing or unreadable: %w", err)
	}
	if mode != "send" {
		return sum, nil
	}
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, a.server()+"/api/agent/server-backups/"+url.PathEscape(name), f)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+a.state.AgentToken)
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if err := a.fence(resp); err != nil {
		return "", err
	}
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("the server refused the upload (HTTP %d)", resp.StatusCode)
	}
	return sum, nil
}
