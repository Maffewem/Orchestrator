// Command orchestrator-livetest runs checks against a real, running Orchestrator server and its
// real devices. It exists for the features the unit tests can only exercise against fakes
// (Docker, Caddy, Cloudflare, GitHub, S3). Everything it creates is named "livetest-*" and is
// removed again; see the README in this directory for what each check needs.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
)

// errSkip marks a check that cannot run here (missing configuration, no human to ask).
type skipError struct{ why string }

func (e skipError) Error() string { return e.why }

func skip(format string, a ...any) error { return skipError{fmt.Sprintf(format, a...)} }

// check is one live test. Needs lists the environment variables it cannot run without.
type check struct {
	Name   string
	Desc   string
	Needs  []string
	Manual bool // needs a human at the keyboard (browser sign-in, pushing a commit, ...)
	Run    func(t *T) error
}

var checks []check

func register(c check) { checks = append(checks, c) }

func main() {
	var (
		list    = flag.Bool("list", false, "list the checks, what each needs and whether it can run, then exit")
		only    = flag.String("run", "", "run only checks whose name matches this regular expression")
		yes     = flag.Bool("yes", false, "never prompt: manual checks are skipped (use in CI)")
		verbose = flag.Bool("v", false, "print each step as it happens")
	)
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: orchestrator-livetest [-list] [-run regexp] [-yes] [-v]\n\nRequired: ORCH_URL and ORCH_ADMIN_TOKEN. See cmd/orchestrator-livetest/README.md.\n\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	sort.SliceStable(checks, func(i, j int) bool { return checks[i].Name < checks[j].Name })
	var re *regexp.Regexp
	if *only != "" {
		var err error
		if re, err = regexp.Compile(*only); err != nil {
			fatal("bad -run pattern: %v", err)
		}
	}

	if *list {
		for _, c := range checks {
			kind := "auto  "
			if c.Manual {
				kind = "manual"
			}
			state := "ready"
			if m := missing(c.Needs); len(m) > 0 {
				state = "needs " + strings.Join(m, ", ")
			}
			fmt.Printf("%-28s %s  %-8s %s\n", c.Name, kind, state, c.Desc)
		}
		return
	}

	base := strings.TrimRight(os.Getenv("ORCH_URL"), "/")
	token := os.Getenv("ORCH_ADMIN_TOKEN")
	if base == "" || token == "" {
		fatal("set ORCH_URL (e.g. http://192.168.1.10:8080) and ORCH_ADMIN_TOKEN")
	}
	c := &client{base: base, token: token, http: &http.Client{Timeout: 60 * time.Second}}
	if _, err := c.call("GET", "/api/health", nil, nil); err != nil {
		fatal("server not reachable at %s: %v", base, err)
	}
	if _, err := c.call("GET", "/api/devices", nil, nil); err != nil {
		fatal("the admin token was not accepted: %v", err)
	}
	cleanStale(c)

	interactive := !*yes
	if fi, err := os.Stdin.Stat(); err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		interactive = false
	}

	var pass, fail, skipped int
	for _, ck := range checks {
		if re != nil && !re.MatchString(ck.Name) {
			continue
		}
		if m := missing(ck.Needs); len(m) > 0 {
			fmt.Printf("SKIP  %-28s needs %s\n", ck.Name, strings.Join(m, ", "))
			skipped++
			continue
		}
		if ck.Manual && !interactive {
			fmt.Printf("SKIP  %-28s manual check, no terminal to prompt on\n", ck.Name)
			skipped++
			continue
		}
		t := &T{c: c, name: ck.Name, interactive: interactive, verbose: *verbose}
		start := time.Now()
		err := t.run(ck)
		took := time.Since(start).Round(time.Second)
		var se skipError
		switch {
		case err == nil:
			fmt.Printf("PASS  %-28s %s\n", ck.Name, took)
			pass++
		case errors.As(err, &se):
			fmt.Printf("SKIP  %-28s %s\n", ck.Name, se.why)
			skipped++
		default:
			fmt.Printf("FAIL  %-28s %v\n", ck.Name, err)
			fail++
		}
	}
	fmt.Printf("\n%d passed, %d failed, %d skipped\n", pass, fail, skipped)
	if fail > 0 {
		os.Exit(1)
	}
}

func fatal(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "orchestrator-livetest: "+format+"\n", a...)
	os.Exit(2)
}

func missing(vars []string) []string {
	var m []string
	for _, v := range vars {
		if os.Getenv(v) == "" {
			m = append(m, v)
		}
	}
	return m
}

// client talks to the control plane with the admin token.
type client struct {
	base, token string
	http        *http.Client
}

// call sends a JSON request and decodes a JSON reply into out (when given). A status of 300 or
// more is returned as an error carrying the server's message.
func (c *client) call(method, path string, body, out any) (int, error) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, c.base+path, rd)
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		msg := strings.TrimSpace(string(raw))
		if json.Unmarshal(raw, &e) == nil && e.Error != "" {
			msg = e.Error
		}
		return resp.StatusCode, fmt.Errorf("%s %s: %d %s", method, path, resp.StatusCode, msg)
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return resp.StatusCode, fmt.Errorf("%s %s: unreadable reply: %v", method, path, err)
		}
	}
	return resp.StatusCode, nil
}

// cleanStale removes projects left behind by an earlier run that was interrupted.
func cleanStale(c *client) {
	var ps []struct{ ID, Name string }
	if _, err := c.call("GET", "/api/projects", nil, &ps); err != nil {
		return
	}
	for _, p := range ps {
		if strings.HasPrefix(p.Name, "livetest-") {
			fmt.Printf("note  removing leftover project %s from an earlier run\n", p.Name)
			c.call("DELETE", "/api/projects/"+p.ID+"?remove_container=1", nil, nil)
		}
	}
}
