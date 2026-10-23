package agent

import (
	"context"
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
func (a *Agent) storeServerBackup(ctx context.Context, name, dir, keep string) error {
	if name == "" || name != filepath.Base(name) || !strings.HasPrefix(name, "orchestrator-") {
		return fmt.Errorf("invalid backup name %q", name)
	}
	if !filepath.IsAbs(dir) {
		return fmt.Errorf("folder %q is not an absolute path", dir)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.cfg.Server+"/api/agent/server-backups/"+url.PathEscape(name), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+a.state.AgentToken)
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("could not download the backup (HTTP %d)", resp.StatusCode)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	dst := filepath.Join(dir, name)
	f, err := os.OpenFile(dst+".part", os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, err = io.Copy(f, resp.Body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(dst + ".part")
		return err
	}
	if err := os.Rename(dst+".part", dst); err != nil {
		return err
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
	return nil
}
