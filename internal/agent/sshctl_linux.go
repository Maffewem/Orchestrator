package agent

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Maffewem/Orchestrator/internal/api"
)

// SSH control. The agent manages one sshd drop-in file (00-orchestrator.conf, which sshd reads before
// the others, and sshd uses the first value it sees) and the authorized_keys of login accounts.
//
// Safety, because a mistake here locks people out of a headless device:
//   - every change is checked first; one that could leave nobody able to sign in needs an explicit confirmation;
//   - the new configuration is validated with `sshd -t` and read back with `sshd -T` before it is kept;
//   - a change is on trial for a few minutes and reverts by itself unless it is confirmed;
//   - existing SSH sessions survive a reload, and the console in the dashboard does not depend on sshd.

type sshCtl struct {
	mu      sync.Mutex
	passwd  string
	dropIn  string // the managed sshd config file
	pending string // where a change on trial is recorded
	run     func(ctx context.Context, name string, args ...string) ([]byte, error)
	now     func() time.Time
	chown   bool // false in tests, which do not run as root
}

func newSSHCtl(stateDir string) *sshCtl {
	return &sshCtl{
		passwd:  "/etc/passwd",
		dropIn:  "/etc/ssh/sshd_config.d/00-orchestrator.conf",
		pending: filepath.Join(stateDir, "ssh-pending.json"),
		run: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			return exec.CommandContext(ctx, name, args...).CombinedOutput()
		},
		now:   time.Now,
		chown: os.Getuid() == 0,
	}
}

type sshPendingFile struct {
	RevertAt time.Time `json:"revert_at"`
	HadFile  bool      `json:"had_file"`
	Old      string    `json:"old"`
}

// --- reading the system ---

func (c *sshCtl) users() []api.SSHUser {
	f, err := os.Open(c.passwd)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []api.SSHUser
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		p := strings.Split(sc.Text(), ":")
		if len(p) < 7 || strings.HasPrefix(p[0], "#") {
			continue
		}
		uid, err := strconv.Atoi(p[2])
		if err != nil || (uid != 0 && (uid < 1000 || uid >= 65534)) {
			continue
		}
		sh := p[6]
		if sh == "" || strings.HasSuffix(sh, "nologin") || strings.HasSuffix(sh, "false") || !filepath.IsAbs(p[5]) {
			continue
		}
		out = append(out, api.SSHUser{Name: p[0], UID: uid, Home: p[5]})
	}
	return out
}

func keysPath(u api.SSHUser) string { return filepath.Join(u.Home, ".ssh", "authorized_keys") }

var sshKeyRe = regexp.MustCompile(`^(ssh-ed25519|ssh-rsa|ecdsa-sha2-nistp(?:256|384|521)|sk-ssh-ed25519@openssh\.com|sk-ecdsa-sha2-nistp256@openssh\.com) ([A-Za-z0-9+/]+={0,2})(?: ([^\x00-\x1f\x7f]{0,120}))?$`)

// parseKey checks one authorized_keys line and returns its fingerprint. Lines with options
// (command=, from= and so on) are not parsed: they are left alone and never offered for removal.
func parseKey(line string) (typ, fp, comment string, ok bool) {
	m := sshKeyRe.FindStringSubmatch(strings.TrimSpace(line))
	if m == nil {
		return "", "", "", false
	}
	blob, err := base64.StdEncoding.DecodeString(m[2])
	if err != nil || len(blob) < 8 {
		return "", "", "", false
	}
	n := binary.BigEndian.Uint32(blob[:4])
	if int(n) > len(blob)-4 || string(blob[4:4+n]) != m[1] {
		return "", "", "", false // the blob names a different type than the line
	}
	sum := sha256.Sum256(blob)
	return m[1], "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:]), m[3], true
}

func (c *sshCtl) keysOf(u api.SSHUser) []api.SSHKey {
	b, err := os.ReadFile(keysPath(u))
	if err != nil {
		return nil
	}
	var out []api.SSHKey
	for _, l := range strings.Split(string(b), "\n") {
		if t, fp, cm, ok := parseKey(l); ok {
			out = append(out, api.SSHKey{User: u.Name, Type: t, Fingerprint: fp, Comment: cm})
		}
	}
	return out
}

func (c *sshCtl) allKeys() (users []api.SSHUser, keys []api.SSHKey) {
	users = c.users()
	for _, u := range users {
		keys = append(keys, c.keysOf(u)...)
	}
	return
}

func (c *sshCtl) sshdBin() string {
	if p, err := exec.LookPath("sshd"); err == nil {
		return p
	}
	if _, err := os.Stat("/usr/sbin/sshd"); err == nil {
		return "/usr/sbin/sshd"
	}
	return ""
}

func (c *sshCtl) service(ctx context.Context) (name string, running bool) {
	for _, n := range []string{"ssh", "sshd"} {
		out, _ := c.run(ctx, "systemctl", "is-active", n)
		if s := strings.TrimSpace(string(out)); s == "active" {
			return n, true
		} else if s == "inactive" || s == "failed" {
			name = n
		}
	}
	return name, false
}

// effective reads the settings sshd would use, from `sshd -T`.
func (c *sshCtl) effective(ctx context.Context) (api.SSHSettings, error) {
	bin := c.sshdBin()
	if bin == "" {
		return api.SSHSettings{}, errors.New("OpenSSH server (sshd) is not installed")
	}
	out, err := c.run(ctx, bin, "-T")
	if err != nil {
		return api.SSHSettings{}, fmt.Errorf("sshd -T: %s", strings.TrimSpace(string(out)))
	}
	var s api.SSHSettings
	for _, l := range strings.Split(string(out), "\n") {
		k, v, _ := strings.Cut(strings.TrimSpace(l), " ")
		switch k {
		case "passwordauthentication":
			s.PasswordAuth = v == "yes"
		case "pubkeyauthentication":
			s.PubkeyAuth = v == "yes"
		case "permitrootlogin":
			if v == "without-password" {
				v = "prohibit-password"
			}
			s.RootLogin = v
		}
	}
	return s, nil
}

func (c *sshCtl) pendingState() (*sshPendingFile, error) {
	b, err := os.ReadFile(c.pending)
	if err != nil {
		return nil, err
	}
	var p sshPendingFile
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

func (c *sshCtl) status(ctx context.Context) api.SSHStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	st := api.SSHStatus{Installed: c.sshdBin() != ""}
	if !st.Installed {
		st.Error = "OpenSSH server (sshd) is not installed on this device."
		return st
	}
	st.Service, st.Running = c.service(ctx)
	eff, err := c.effective(ctx)
	if err != nil {
		st.Error = err.Error()
	}
	st.Effective = eff
	_, e := os.Stat(c.dropIn)
	st.Managed = e == nil
	st.Users, st.Keys = c.allKeys()
	if p, err := c.pendingState(); err == nil {
		st.Pending = &api.SSHPending{RevertAt: p.RevertAt.UTC().Format(time.RFC3339)}
	}
	return st
}

// --- safety checks ---

// signIns counts the accounts that could sign in with a key under the given settings.
func signIns(users []api.SSHUser, keys []api.SSHKey, s api.SSHSettings) []string {
	if !s.PubkeyAuth {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, k := range keys {
		if k.User == "root" && s.RootLogin == "no" {
			continue
		}
		if !seen[k.User] {
			seen[k.User] = true
			out = append(out, k.User)
		}
	}
	return out
}

func checkSettings(users []api.SSHUser, keys []api.SSHKey, cur, want api.SSHSettings) api.SSHCheck {
	var ck api.SSHCheck
	add := func(level, msg string) {
		ck.Warnings = append(ck.Warnings, api.SSHWarning{Level: level, Message: msg})
		if level == "lockout" {
			ck.Lockout = true
		}
	}
	if !want.PasswordAuth {
		who := signIns(users, keys, want)
		switch {
		case !want.PubkeyAuth:
			add("lockout", "Password and key sign-in would both be off, so nobody could sign in over SSH.")
		case len(who) == 0:
			msg := "No account has an authorized key that would still work, so turning password sign-in off would leave nobody able to sign in over SSH."
			if want.RootLogin == "no" && len(keys) > 0 {
				msg += " (Keys for root don't count while root login is off.)"
			}
			add("lockout", msg)
		case len(who) == 1:
			add("note", fmt.Sprintf("Only %s has a key. Make sure you hold its private key before you continue.", who[0]))
		}
	}
	if want.RootLogin == "yes" && want.PasswordAuth {
		add("note", "Root can sign in with a password. Prefer keys only, or prohibit-password.")
	}
	if cur.PasswordAuth && !want.PasswordAuth {
		add("note", "Anyone signed in now stays connected, but new password sign-ins stop.")
	}
	return ck
}

func validSettings(s api.SSHSettings) error {
	switch s.RootLogin {
	case "yes", "no", "prohibit-password", "forced-commands-only":
		return nil
	}
	return fmt.Errorf("root_login must be yes, no, prohibit-password or forced-commands-only (got %q)", s.RootLogin)
}

func (c *sshCtl) check(ctx context.Context, want api.SSHSettings) (api.SSHCheck, error) {
	if err := validSettings(want); err != nil {
		return api.SSHCheck{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	cur, err := c.effective(ctx)
	if err != nil {
		return api.SSHCheck{}, err
	}
	users, keys := c.allKeys()
	return checkSettings(users, keys, cur, want), nil
}

// --- changing sshd's settings ---

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func renderDropIn(s api.SSHSettings) string {
	return fmt.Sprintf("# Managed by Orchestrator. Delete this file (or use Revert in the dashboard) to go back to the system's settings.\nPasswordAuthentication %s\nPubkeyAuthentication %s\nPermitRootLogin %s\n",
		yesNo(s.PasswordAuth), yesNo(s.PubkeyAuth), s.RootLogin)
}

func writeAtomic(path string, data []byte, mode os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (c *sshCtl) reload(ctx context.Context) error {
	var last string
	for _, n := range []string{"ssh", "sshd"} {
		out, err := c.run(ctx, "systemctl", "reload", n)
		if err == nil {
			return nil
		}
		last = strings.TrimSpace(string(out))
	}
	return fmt.Errorf("could not reload sshd: %s", last)
}

// restore puts the drop-in back as it was and reloads. Callers hold mu.
func (c *sshCtl) restore(ctx context.Context, hadFile bool, old string) error {
	if hadFile {
		if err := writeAtomic(c.dropIn, []byte(old), 0o644); err != nil {
			return err
		}
	} else if err := os.Remove(c.dropIn); err != nil && !os.IsNotExist(err) {
		return err
	}
	return c.reload(ctx)
}

// apply changes sshd's settings. trial is how many seconds the change stays on trial.
func (c *sshCtl) apply(ctx context.Context, want api.SSHSettings, confirm bool, trial int) (string, error) {
	if err := validSettings(want); err != nil {
		return "", err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, err := c.pendingState(); err == nil {
		return "", errors.New("an earlier SSH change is still on trial: confirm or revert it first")
	}
	bin := c.sshdBin()
	if bin == "" {
		return "", errors.New("OpenSSH server (sshd) is not installed")
	}
	cur, err := c.effective(ctx)
	if err != nil {
		return "", err
	}
	users, keys := c.allKeys()
	if ck := checkSettings(users, keys, cur, want); ck.Lockout && !confirm {
		return "", errors.New("refused: this could lock everyone out of SSH, and it was not confirmed")
	}
	old, rerr := os.ReadFile(c.dropIn)
	hadFile := rerr == nil
	if err := os.MkdirAll(filepath.Dir(c.dropIn), 0o755); err != nil {
		return "", err
	}
	if err := writeAtomic(c.dropIn, []byte(renderDropIn(want)), 0o644); err != nil {
		return "", err
	}
	undo := func() { c.restoreFile(hadFile, string(old)) }
	if out, err := c.run(ctx, bin, "-t"); err != nil {
		undo()
		return "", fmt.Errorf("sshd rejected the new configuration, so nothing was changed: %s", strings.TrimSpace(string(out)))
	}
	got, err := c.effective(ctx)
	if err != nil {
		undo()
		return "", err
	}
	if got != want {
		undo()
		return "", errors.New("sshd did not use the new settings: another file (or a Match block) overrides them, or sshd_config has no Include for /etc/ssh/sshd_config.d. Nothing was changed")
	}
	if err := c.reload(ctx); err != nil {
		undo()
		return "", err
	}
	if trial > 0 {
		if trial > 600 {
			trial = 600
		}
		p := sshPendingFile{RevertAt: c.now().Add(time.Duration(trial) * time.Second), HadFile: hadFile, Old: string(old)}
		os.MkdirAll(filepath.Dir(c.pending), 0o755)
		b, _ := json.Marshal(p)
		if err := os.WriteFile(c.pending, b, 0o600); err != nil {
			undo()
			c.reload(ctx)
			return "", err
		}
		return fmt.Sprintf("changed; reverts in %d seconds unless confirmed", trial), nil
	}
	return "changed", nil
}

func (c *sshCtl) restoreFile(hadFile bool, old string) {
	if hadFile {
		writeAtomic(c.dropIn, []byte(old), 0o644)
	} else {
		os.Remove(c.dropIn)
	}
}

// confirm keeps a change on trial; revert undoes it now.
func (c *sshCtl) confirm(ctx context.Context, revert bool) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	p, err := c.pendingState()
	if err != nil {
		if revert {
			// Nothing on trial: "revert" still removes the managed file, a way back to the system's settings.
			if err := c.restore(ctx, false, ""); err != nil {
				return "", err
			}
			return "the managed settings were removed", nil
		}
		return "", errors.New("no SSH change is on trial")
	}
	if !revert {
		os.Remove(c.pending)
		return "confirmed", nil
	}
	if err := c.restore(ctx, p.HadFile, p.Old); err != nil {
		return "", err
	}
	os.Remove(c.pending)
	return "reverted", nil
}

// watch reverts a change that was not confirmed in time. It also covers an agent restart mid-trial.
func (c *sshCtl) watch(ctx context.Context, log func(string, ...any)) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		c.mu.Lock()
		if p, err := c.pendingState(); err == nil && !c.now().Before(p.RevertAt) {
			if err := c.restore(ctx, p.HadFile, p.Old); err != nil {
				log("ssh: reverting the unconfirmed change failed", "err", err)
			} else {
				os.Remove(c.pending)
				log("ssh: an unconfirmed change was reverted")
			}
		}
		c.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// --- authorized_keys ---

func (c *sshCtl) findUser(name string) (api.SSHUser, bool) {
	for _, u := range c.users() {
		if u.Name == name {
			return u, true
		}
	}
	return api.SSHUser{}, false
}

func notLink(p string) error {
	fi, err := os.Lstat(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symbolic link; refusing to write through it", p)
	}
	return nil
}

func (c *sshCtl) writeKeys(u api.SSHUser, content string) error {
	dir := filepath.Join(u.Home, ".ssh")
	file := filepath.Join(dir, "authorized_keys")
	if err := notLink(dir); err != nil {
		return err
	}
	if err := notLink(file); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp := file + ".orchestrator.tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o600); err != nil {
		return err
	}
	if c.chown {
		fi, err := os.Stat(u.Home)
		if err != nil {
			os.Remove(tmp)
			return err
		}
		if st, ok := fi.Sys().(*syscall.Stat_t); ok {
			os.Chown(dir, int(st.Uid), int(st.Gid))
			os.Chown(tmp, int(st.Uid), int(st.Gid))
		}
	}
	return os.Rename(tmp, file)
}

func (c *sshCtl) addKey(user, line string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	line = strings.TrimSpace(line)
	if strings.ContainsAny(line, "\r\n") {
		return "", errors.New("paste one public key, on one line")
	}
	_, fp, _, ok := parseKey(line)
	if !ok {
		return "", errors.New("that is not a public key this agent accepts (ssh-ed25519, ssh-rsa, ecdsa-sha2-nistp*, sk-*), or it carries options")
	}
	u, ok := c.findUser(user)
	if !ok {
		return "", fmt.Errorf("%q is not an account that can sign in on this device", user)
	}
	for _, k := range c.keysOf(u) {
		if k.Fingerprint == fp {
			return "", errors.New("that key is already authorized for " + user)
		}
	}
	existing, _ := os.ReadFile(keysPath(u))
	s := string(existing)
	if s != "" && !strings.HasSuffix(s, "\n") {
		s += "\n"
	}
	if err := c.writeKeys(u, s+line+"\n"); err != nil {
		return "", err
	}
	return fp, nil
}

func (c *sshCtl) removeKey(ctx context.Context, user, fp string, confirm bool) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	u, ok := c.findUser(user)
	if !ok {
		return "", fmt.Errorf("%q is not an account that can sign in on this device", user)
	}
	b, err := os.ReadFile(keysPath(u))
	if err != nil {
		return "", errors.New("that account has no authorized keys")
	}
	var kept []string
	removed := 0
	for _, l := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		if _, f, _, ok := parseKey(l); ok && f == fp {
			removed++
			continue
		}
		kept = append(kept, l)
	}
	if removed == 0 {
		return "", errors.New("that key is not in the file (it may already be removed, or it carries options and is left alone)")
	}
	// Would this leave nobody able to sign in?
	if eff, err := c.effective(ctx); err == nil && !eff.PasswordAuth && !confirm {
		users, all := c.allKeys()
		var rest []api.SSHKey
		for _, k := range all {
			if !(k.User == user && k.Fingerprint == fp) {
				rest = append(rest, k)
			}
		}
		if len(signIns(users, rest, eff)) == 0 {
			return "", errors.New("refused: password sign-in is off and this is the last working key, so removing it would lock everyone out of SSH. It was not confirmed")
		}
	}
	out := strings.Join(kept, "\n")
	if out != "" {
		out += "\n"
	}
	if err := c.writeKeys(u, out); err != nil {
		return "", err
	}
	return "removed", nil
}
