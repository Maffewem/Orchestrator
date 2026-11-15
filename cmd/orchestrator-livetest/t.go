package main

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// T is the context of one running check: API access, cleanup, prompts and polling helpers.
type T struct {
	c           *client
	name        string
	interactive bool
	verbose     bool
	cleanups    []func()
}

func (t *T) run(ck check) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
		for i := len(t.cleanups) - 1; i >= 0; i-- {
			t.cleanups[i]()
		}
	}()
	return ck.Run(t)
}

func (t *T) logf(format string, a ...any) {
	if t.verbose {
		fmt.Printf("      %s\n", fmt.Sprintf(format, a...))
	}
}

// cleanup registers something to undo when the check ends, pass or fail.
func (t *T) cleanup(f func()) { t.cleanups = append(t.cleanups, f) }

func (t *T) api(method, path string, body, out any) error {
	_, err := t.c.call(method, path, body, out)
	return err
}

// ask shows an instruction and waits for the human to confirm (y) or decline (n).
func (t *T) ask(format string, a ...any) error {
	if !t.interactive {
		return skip("needs a person at the terminal")
	}
	fmt.Printf("\n  >> %s\n     Done? [y/N] ", fmt.Sprintf(format, a...))
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(line)), "y") {
		return fmt.Errorf("not confirmed")
	}
	return nil
}

// confirm asks a yes/no question about what the human saw.
func (t *T) confirm(question string) error {
	if !t.interactive {
		return skip("needs a person at the terminal")
	}
	fmt.Printf("\n  ?? %s [y/N] ", question)
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(line)), "y") {
		return fmt.Errorf("answered no: %s", question)
	}
	return nil
}

// until polls f every two seconds until it returns true, an error, or the time runs out.
func (t *T) until(what string, limit time.Duration, f func() (bool, error)) error {
	deadline := time.Now().Add(limit)
	for {
		ok, err := f()
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out after %s waiting for %s", limit, what)
		}
		time.Sleep(2 * time.Second)
	}
}

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// ---- devices ----

type device struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Status      string `json:"status"`
	Proxy       string `json:"proxy"`
	Maintenance bool   `json:"maintenance"`
	Host        struct {
		Addresses    []string `json:"addresses"`
		Capabilities []string `json:"capabilities"`
	} `json:"host"`
	Containers []struct {
		ID    string `json:"id"`
		Name  string `json:"name"`
		State string `json:"state"`
	} `json:"containers"`
}

func (d *device) addr() string {
	for _, a := range d.Host.Addresses {
		if !strings.Contains(a, ":") && !strings.HasPrefix(a, "172.17.") {
			return a
		}
	}
	return ""
}

func (d *device) has(cap string) bool {
	for _, c := range d.Host.Capabilities {
		if c == cap {
			return true
		}
	}
	return false
}

func (d *device) container(name string) (id, state string) {
	for _, c := range d.Containers {
		if c.Name == name {
			return c.ID, c.State
		}
	}
	return "", ""
}

// findDevice returns the device named by the environment variable (a name or an ID), or the
// first online device when it is empty.
func (t *T) findDevice(envVar string) (*device, error) {
	var ds []device
	if err := t.api("GET", "/api/devices", nil, &ds); err != nil {
		return nil, err
	}
	want := os.Getenv(envVar)
	for i := range ds {
		d := &ds[i]
		if want == "" && d.Status == "online" || want != "" && (d.ID == want || strings.EqualFold(d.Name, want)) {
			if d.Status != "online" {
				return nil, fmt.Errorf("device %s is offline", d.Name)
			}
			if d.addr() == "" {
				return nil, fmt.Errorf("device %s reports no usable IP address", d.Name)
			}
			return d, nil
		}
	}
	if want != "" {
		return nil, fmt.Errorf("no device %q (set %s to a device name or ID)", want, envVar)
	}
	return nil, skip("no online device; enroll one first")
}

func (t *T) device(id string) (*device, error) {
	var d device
	return &d, t.api("GET", "/api/devices/"+id, nil, &d)
}

// ---- projects and deployments ----

type project struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	DeviceID string `json:"device_id"`
}

type deployment struct {
	ID      string `json:"id"`
	Status  string `json:"status"`
	Error   string `json:"error"`
	URL     string `json:"url"`
	Trigger string `json:"trigger"`
	Steps   []struct {
		Name   string `json:"name"`
		Status string `json:"status"`
		Log    string `json:"log"`
	} `json:"steps"`
}

// newProject creates a livetest-* project and removes it (and its container) when the check ends.
func (t *T) newProject(suffix, deviceID string, source, runtime map[string]any) (*project, error) {
	name := "livetest-" + suffix + "-" + randHex(2)
	if runtime == nil {
		runtime = map[string]any{}
	}
	if runtime["restart"] == nil {
		runtime["restart"] = "unless-stopped"
	}
	if runtime["ports"] == nil {
		runtime["ports"] = []string{}
	}
	if runtime["env"] == nil {
		runtime["env"] = []string{}
	}
	var p project
	err := t.api("POST", "/api/projects", map[string]any{"name": name, "device_id": deviceID, "source": source, "runtime": runtime}, &p)
	if err != nil {
		return nil, err
	}
	t.cleanup(func() { t.c.call("DELETE", "/api/projects/"+p.ID+"?remove_container=1", nil, nil) })
	return &p, nil
}

// deployAndWait starts a deployment and waits for it to finish; it fails with the failing step's log.
func (t *T) deployAndWait(p *project, limit time.Duration) (*deployment, error) {
	var d deployment
	if err := t.api("POST", "/api/projects/"+p.ID+"/deployments", nil, &d); err != nil {
		return nil, err
	}
	return t.waitDeployment(d.ID, limit)
}

func (t *T) waitDeployment(id string, limit time.Duration) (*deployment, error) {
	var d deployment
	err := t.until("deployment "+id, limit, func() (bool, error) {
		if err := t.api("GET", "/api/deployments/"+id, nil, &d); err != nil {
			return false, err
		}
		switch d.Status {
		case "succeeded":
			return true, nil
		case "failed":
			msg := d.Error
			for _, s := range d.Steps {
				if s.Status == "failed" {
					msg = s.Name + ": " + lastLines(s.Log, 6)
				}
			}
			return false, fmt.Errorf("deployment failed: %s", msg)
		}
		return false, nil
	})
	return &d, err
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " | ")
}

// command queues an agent command and waits for its result.
func (t *T) command(deviceID, action, target string) error {
	var cmd struct {
		ID string `json:"id"`
	}
	if err := t.api("POST", "/api/devices/"+deviceID+"/commands", map[string]any{"action": action, "target": target}, &cmd); err != nil {
		return err
	}
	return t.until(action+" to finish", 90*time.Second, func() (bool, error) {
		var c struct{ Status, Error string }
		if err := t.api("GET", "/api/commands/"+cmd.ID, nil, &c); err != nil {
			return false, err
		}
		switch c.Status {
		case "succeeded":
			return true, nil
		case "failed", "cancelled":
			return false, fmt.Errorf("%s %s: %s", action, c.Status, c.Error)
		}
		return false, nil
	})
}

// ---- plain HTTP probes (not through the control plane) ----

var probe = &http.Client{
	Timeout:       10 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// get fetches url, optionally with a Host header and basic auth, and returns status and body.
func get(url, host, user, pass string) (int, string, error) {
	req, _ := http.NewRequest("GET", url, nil)
	if host != "" {
		req.Host = host
	}
	if user != "" {
		req.SetBasicAuth(user, pass)
	}
	resp, err := probe.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, string(b), nil
}

// waitHTTP waits until url answers with want (and optionally a body containing text).
func (t *T) waitHTTP(url, host, text string, want int, limit time.Duration) error {
	var last string
	err := t.until(url+" to answer "+fmt.Sprint(want), limit, func() (bool, error) {
		code, body, err := get(url, host, "", "")
		if err != nil {
			last = err.Error()
			return false, nil
		}
		last = fmt.Sprintf("status %d", code)
		return code == want && strings.Contains(body, text), nil
	})
	if err != nil {
		return fmt.Errorf("%v (last: %s)", err, last)
	}
	return nil
}
