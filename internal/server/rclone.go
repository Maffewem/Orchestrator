package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// Cloud storage destinations (Google Drive, Dropbox, OneDrive and many others) are served by
// rclone. rclone does the OAuth token refresh and chunked uploads; the dashboard only holds the
// configuration section produced by `rclone config`. The binary is bundled in the server image;
// ORCH_RCLONE points at a different one.

func rcloneBinary() string {
	if p := os.Getenv("ORCH_RCLONE"); p != "" {
		return p
	}
	return "rclone"
}

// rcloneRemoteName returns the name of the first [section] in an rclone config.
func rcloneRemoteName(conf string) string {
	for _, l := range strings.Split(conf, "\n") {
		l = strings.TrimSpace(l)
		if strings.HasPrefix(l, "[") && strings.HasSuffix(l, "]") {
			return strings.TrimSpace(l[1 : len(l)-1])
		}
	}
	return ""
}

// validateRcloneConf rejects configurations that would make rclone run commands on the server or
// that are not a remote at all.
func validateRcloneConf(conf string) error {
	if rcloneRemoteName(conf) == "" {
		return errors.New("paste the section from `rclone config show`, starting with a [name] line")
	}
	for _, l := range strings.Split(conf, "\n") {
		k, _, ok := strings.Cut(l, "=")
		if !ok {
			continue
		}
		k = strings.ToLower(strings.TrimSpace(k))
		if k == "ssh" || k == "command" || strings.HasSuffix(k, "_command") {
			return fmt.Errorf("the option %q runs a command on the server and is not allowed", k)
		}
	}
	return nil
}

func (d BackupDest) rcloneRemote() string {
	if d.Remote != "" {
		return d.Remote
	}
	return rcloneRemoteName(d.RcloneConf)
}

// rcloneRun runs rclone with the destination's configuration in a private temporary file. If
// rclone refreshed an OAuth token it rewrites that file; the new text is returned so it can be
// stored (some providers rotate refresh tokens).
func (d BackupDest) rcloneRun(ctx context.Context, args ...string) (stdout string, newConf string, err error) {
	bin, err := exec.LookPath(rcloneBinary())
	if err != nil {
		return "", "", errors.New("rclone is not installed on the server. The official image includes it; otherwise install rclone or set ORCH_RCLONE")
	}
	dir, err := os.MkdirTemp("", "orch-rclone-*")
	if err != nil {
		return "", "", err
	}
	defer os.RemoveAll(dir)
	conf := filepath.Join(dir, "rclone.conf")
	if err := os.WriteFile(conf, []byte(d.RcloneConf), 0o600); err != nil {
		return "", "", err
	}
	cmd := exec.CommandContext(ctx, bin, append([]string{"--config", conf, "--retries", "3"}, args...)...)
	cmd.Env = []string{"HOME=" + dir, "PATH=" + os.Getenv("PATH"), "SSL_CERT_FILE=" + os.Getenv("SSL_CERT_FILE")}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	runErr := cmd.Run()
	if b, rerr := os.ReadFile(conf); rerr == nil && string(b) != d.RcloneConf {
		newConf = string(b)
	}
	if runErr != nil {
		msg := strings.TrimSpace(errb.String())
		if len(msg) > 600 {
			msg = msg[len(msg)-600:]
		}
		if msg == "" {
			msg = runErr.Error()
		}
		return "", newConf, fmt.Errorf("rclone: %s", msg)
	}
	return out.String(), newConf, nil
}

func (d BackupDest) rcloneTarget(name string) string {
	return d.rcloneRemote() + ":" + strings.Trim(path.Join(d.Path, name), "/")
}

// rcloneSend uploads one file and keeps the newest keep backups in the folder.
func (s *Server) rcloneSend(ctx context.Context, d BackupDest, name, src string, keep int) error {
	_, nc, err := d.rcloneRun(ctx, "copyto", src, d.rcloneTarget(name))
	s.storeRcloneConf(ctx, d.ID, nc)
	if err != nil {
		return err
	}
	out, nc, err := d.rcloneRun(ctx, "lsf", "--files-only", d.rcloneTarget(""))
	s.storeRcloneConf(ctx, d.ID, nc)
	if err != nil {
		return nil // uploaded; pruning is best effort
	}
	for _, n := range oldBackupNames(out, keep) {
		d.rcloneRun(ctx, "deletefile", d.rcloneTarget(n))
	}
	return nil
}

// oldBackupNames lists the server backups beyond the newest keep in an rclone listing.
func oldBackupNames(listing string, keep int) []string {
	var names []string
	for _, n := range strings.Split(listing, "\n") {
		n = strings.TrimSpace(n)
		if strings.HasPrefix(n, serverBackupPrefix) && (strings.HasSuffix(n, ".db") || strings.HasSuffix(n, ".db.enc")) {
			names = append(names, n)
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names))) // names carry a UTC timestamp
	if len(names) <= keep {
		return nil
	}
	return names[keep:]
}

func (s *Server) rcloneTest(ctx context.Context, d BackupDest, probe, local string) error {
	_, nc, err := d.rcloneRun(ctx, "copyto", local, d.rcloneTarget(probe))
	s.storeRcloneConf(ctx, d.ID, nc)
	if err != nil {
		return err
	}
	_, nc, err = d.rcloneRun(ctx, "deletefile", d.rcloneTarget(probe))
	s.storeRcloneConf(ctx, d.ID, nc)
	return err
}

// storeRcloneConf saves a configuration rclone rewrote (a refreshed token), if any.
func (s *Server) storeRcloneConf(ctx context.Context, id, conf string) {
	if conf == "" {
		return
	}
	cur := s.backupDests(ctx)
	for i := range cur {
		if cur[i].ID == id {
			cur[i].RcloneConf = conf
			s.saveBackupDests(ctx, cur)
			return
		}
	}
}
