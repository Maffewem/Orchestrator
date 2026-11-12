package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

func init() {
	register(check{Name: "core/deploy-image", Desc: "deploy nginx to a real device and fetch the page", Run: checkDeployImage})
	register(check{Name: "core/maintenance-mode", Desc: "maintenance mode blocks new workers and lifts again", Run: checkMaintenance})
	register(check{Name: "lifecycle/stop-start", Desc: "stop and start a container through the agent", Run: checkStopStart})
	register(check{Name: "streams/logs", Desc: "live container logs arrive over the long-poll stream", Run: checkLogs})
	register(check{Name: "streams/shell", Desc: "a shell runs a command inside the container (real exec hijack)", Run: checkShell})
	register(check{Name: "backup/container", Desc: "back up a container, restore it, page still served", Run: checkContainerBackup})
	register(check{Name: "backup/server", Desc: "create a server backup and see it listed with a size", Run: checkServerBackup})
	register(check{Name: "backup/server-restore", Desc: "restore a server backup; the container restarts and the marker returns", Needs: []string{"LIVETEST_SERVER_RESTORE"}, Run: checkServerRestore})
	register(check{Name: "backup/dest-s3", Desc: "replicate server backups to S3 / R2", Needs: []string{"LIVETEST_S3_ENDPOINT", "LIVETEST_S3_BUCKET", "LIVETEST_S3_ACCESS_KEY", "LIVETEST_S3_SECRET_KEY"}, Run: checkDestS3})
	register(check{Name: "backup/dest-device", Desc: "copy server backups to a second device", Needs: []string{"LIVETEST_DEVICE2"}, Run: checkDestDevice})
	register(check{Name: "domains/caddy", Desc: "per-device Caddy proxy answers for a worker's hostname", Needs: []string{"LIVETEST_BASE_DOMAIN"}, Run: checkCaddy})
	register(check{Name: "domains/login-gate", Desc: "forward_auth login gate fails closed and lets a user in", Needs: []string{"LIVETEST_BASE_DOMAIN"}, Run: checkLoginGate})
	register(check{Name: "env/encryption", Desc: "env secrets are not readable in the database file", Needs: []string{"LIVETEST_DB_PATH"}, Run: checkEnvEncryption})
	register(check{Name: "env/fresh-restore", Desc: "restore a backup onto a fresh server (guided)", Manual: true, Run: checkFreshRestore})
	register(check{Name: "adopt/redeploy", Desc: "adopt an existing container and redeploy it", Needs: []string{"LIVETEST_ADOPT_CONTAINER"}, Run: checkAdopt})
	register(check{Name: "migrate/device", Desc: "migrate a worker, with its data, to a second device", Needs: []string{"LIVETEST_DEVICE2"}, Run: checkMigrate})
	register(check{Name: "tunnel/state", Desc: "Cloudflare Tunnel runs on the device and reports routes", Needs: []string{"LIVETEST_TUNNEL"}, Run: checkTunnel})
	register(check{Name: "sso/sign-in", Desc: "SSO provider listed and a browser sign-in works (guided)", Manual: true, Run: checkSSO})
	register(check{Name: "github/webhook-signed", Desc: "a signed push webhook deploys the project", Needs: []string{"LIVETEST_GH_REPO"}, Run: checkWebhookSigned})
	register(check{Name: "github/real-push", Desc: "a real git push triggers a deployment (guided)", Needs: []string{"LIVETEST_GH_REPO"}, Manual: true, Run: checkRealPush})
	register(check{Name: "github/preview-pr", Desc: "PR preview via Caddy and its cleanup (guided)", Needs: []string{"LIVETEST_GH_REPO", "LIVETEST_BASE_DOMAIN"}, Manual: true, Run: checkPreviewPR})
	register(check{Name: "node/build", Desc: "build a Node.js repo into an image on the device", Needs: []string{"LIVETEST_NODE_REPO"}, Run: checkNode})
	register(check{Name: "cloudflare/pages", Desc: "build and deploy a Pages project, see it in Cloudflare", Needs: []string{"LIVETEST_CF_ACCOUNT_ID", "LIVETEST_CF_TOKEN", "LIVETEST_CF_PAGES_REPO"}, Run: checkCFPages})
	register(check{Name: "cloudflare/worker-analytics", Desc: "deploy a Worker and read its analytics (the untested GraphQL query)", Needs: []string{"LIVETEST_CF_ACCOUNT_ID", "LIVETEST_CF_TOKEN", "LIVETEST_CF_WORKER_REPO"}, Run: checkCFWorker})
	register(check{Name: "cloudflare/discover", Desc: "discover existing Pages projects and Workers", Needs: []string{"LIVETEST_CF_ACCOUNT_ID", "LIVETEST_CF_TOKEN"}, Run: checkCFDiscover})
}

func port(offset int) int {
	base := 18080
	if v, err := strconv.Atoi(os.Getenv("LIVETEST_PORT")); err == nil && v > 0 {
		base = v
	}
	return base + offset
}

// nginx deploys a throwaway nginx container and returns it with the device it runs on.
func (t *T) nginx(dev *device, suffix string, p int, extra map[string]any) (*project, error) {
	rt := map[string]any{"ports": []string{fmt.Sprintf("%d:80", p)}}
	for k, v := range extra {
		rt[k] = v
	}
	pr, err := t.newProject(suffix, dev.ID, map[string]any{"type": "image", "image": "nginx:alpine"}, rt)
	if err != nil {
		return nil, err
	}
	if _, err := t.deployAndWait(pr, 4*time.Minute); err != nil {
		return nil, err
	}
	return pr, nil
}

// containerID waits for the agent to report the named container and returns its ID.
func (t *T) containerID(dev *device, name, wantState string) (string, error) {
	var id string
	err := t.until("container "+name+" to be "+wantState, 60*time.Second, func() (bool, error) {
		d, err := t.device(dev.ID)
		if err != nil {
			return false, err
		}
		cid, st := d.container(name)
		id = cid
		return cid != "" && (wantState == "" || st == wantState), nil
	})
	return id, err
}

func checkDeployImage(t *T) error {
	dev, err := t.findDevice("LIVETEST_DEVICE")
	if err != nil {
		return err
	}
	if _, err := t.nginx(dev, "web", port(0), nil); err != nil {
		return err
	}
	return t.waitHTTP(fmt.Sprintf("http://%s:%d/", dev.addr(), port(0)), "", "nginx", 200, 30*time.Second)
}

func checkMaintenance(t *T) error {
	dev, err := t.findDevice("LIVETEST_DEVICE")
	if err != nil {
		return err
	}
	if err := t.api("PATCH", "/api/devices/"+dev.ID, map[string]any{"maintenance": true, "maintenance_note": "livetest"}, nil); err != nil {
		return err
	}
	t.cleanup(func() { t.c.call("PATCH", "/api/devices/"+dev.ID, map[string]any{"maintenance": false}, nil) })
	var p project
	err = t.api("POST", "/api/projects", map[string]any{"name": "livetest-maint", "device_id": dev.ID,
		"source": map[string]any{"type": "image", "image": "nginx:alpine"}, "runtime": map[string]any{"ports": []string{}, "env": []string{}, "restart": "no"}}, &p)
	if err == nil {
		t.c.call("DELETE", "/api/projects/"+p.ID, nil, nil)
		return fmt.Errorf("a new worker was accepted on a device in maintenance")
	}
	t.logf("refused as expected: %v", err)
	if err := t.api("PATCH", "/api/devices/"+dev.ID, map[string]any{"maintenance": false}, nil); err != nil {
		return err
	}
	pr, err := t.newProject("maint", dev.ID, map[string]any{"type": "image", "image": "nginx:alpine"}, map[string]any{"restart": "no"})
	if err != nil {
		return fmt.Errorf("after lifting maintenance: %v", err)
	}
	_ = pr
	return nil
}

func checkStopStart(t *T) error {
	dev, err := t.findDevice("LIVETEST_DEVICE")
	if err != nil {
		return err
	}
	pr, err := t.nginx(dev, "cycle", port(1), nil)
	if err != nil {
		return err
	}
	id, err := t.containerID(dev, pr.Name, "running")
	if err != nil {
		return err
	}
	if err := t.command(dev.ID, "container.stop", id); err != nil {
		return err
	}
	if _, err := t.containerID(dev, pr.Name, "exited"); err != nil {
		return err
	}
	if err := t.command(dev.ID, "container.start", id); err != nil {
		return err
	}
	if _, err := t.containerID(dev, pr.Name, "running"); err != nil {
		return err
	}
	return t.waitHTTP(fmt.Sprintf("http://%s:%d/", dev.addr(), port(1)), "", "nginx", 200, 30*time.Second)
}

type streamChunk struct {
	Data   []byte `json:"data"`
	Next   int64  `json:"next"`
	Closed bool   `json:"closed"`
	Error  string `json:"error"`
}

// readStream polls a stream until its output contains want.
func (t *T) readStream(id, want string, limit time.Duration) error {
	var from int64
	var got strings.Builder
	return t.until("stream output containing "+strconv.Quote(want), limit, func() (bool, error) {
		var c streamChunk
		if err := t.api("GET", fmt.Sprintf("/api/streams/%s/output?from=%d", id, from), nil, &c); err != nil {
			return false, err
		}
		from = c.Next
		got.Write(c.Data)
		if strings.Contains(got.String(), want) {
			return true, nil
		}
		if c.Closed {
			return false, fmt.Errorf("stream ended without %q (error %q, output %q)", want, c.Error, got.String())
		}
		return false, nil
	})
}

func (t *T) openStream(dev *device, kind, container string) (string, error) {
	var s struct {
		ID string `json:"id"`
	}
	if err := t.api("POST", "/api/devices/"+dev.ID+"/streams", map[string]any{"kind": kind, "container": container, "tail": 50}, &s); err != nil {
		return "", err
	}
	t.cleanup(func() { t.c.call("DELETE", "/api/streams/"+s.ID, nil, nil) })
	return s.ID, nil
}

func checkLogs(t *T) error {
	dev, err := t.findDevice("LIVETEST_DEVICE")
	if err != nil {
		return err
	}
	pr, err := t.nginx(dev, "logs", port(2), nil)
	if err != nil {
		return err
	}
	if _, err := t.containerID(dev, pr.Name, "running"); err != nil {
		return err
	}
	id, err := t.openStream(dev, "logs", pr.Name)
	if err != nil {
		return err
	}
	marker := "/livetest-" + randHex(3)
	get(fmt.Sprintf("http://%s:%d%s", dev.addr(), port(2), marker), "", "", "")
	return t.readStream(id, marker, 30*time.Second)
}

func checkShell(t *T) error {
	dev, err := t.findDevice("LIVETEST_DEVICE")
	if err != nil {
		return err
	}
	pr, err := t.nginx(dev, "shell", port(3), nil)
	if err != nil {
		return err
	}
	if _, err := t.containerID(dev, pr.Name, "running"); err != nil {
		return err
	}
	id, err := t.openStream(dev, "shell", pr.Name)
	if err != nil {
		return err
	}
	if err := t.api("POST", "/api/streams/"+id+"/input", map[string]any{"data": []byte("echo live-$((6*7))\n")}, nil); err != nil {
		return err
	}
	return t.readStream(id, "live-42", 30*time.Second)
}

func checkContainerBackup(t *T) error {
	dev, err := t.findDevice("LIVETEST_DEVICE")
	if err != nil {
		return err
	}
	if !dev.has("backup") {
		return skip("agent on %s predates backups; re-run its install command", dev.Name)
	}
	pr, err := t.nginx(dev, "bkp", port(4), map[string]any{"volumes": []string{"livetest-" + randHex(2) + "-data:/usr/share/nginx/html"}})
	if err != nil {
		return err
	}
	id, err := t.containerID(dev, pr.Name, "running")
	if err != nil {
		return err
	}
	var b struct{ ID, Status, Error string }
	if err := t.api("POST", "/api/devices/"+dev.ID+"/containers/"+id+"/backup", nil, &b); err != nil {
		return err
	}
	t.cleanup(func() { t.c.call("DELETE", "/api/backups/"+b.ID, nil, nil) })
	if err := t.until("backup to complete", 3*time.Minute, func() (bool, error) {
		var all []struct {
			ID, Status, Error string
			Size              int64
		}
		if err := t.api("GET", "/api/backups?device_id="+dev.ID, nil, &all); err != nil {
			return false, err
		}
		for _, x := range all {
			if x.ID == b.ID {
				if x.Status == "failed" {
					return false, fmt.Errorf("backup failed: %s", x.Error)
				}
				return x.Status == "complete" && x.Size > 0, nil
			}
		}
		return false, fmt.Errorf("backup %s vanished from the list", b.ID)
	}); err != nil {
		return err
	}
	if err := t.api("POST", "/api/backups/"+b.ID+"/restore", nil, nil); err != nil {
		return err
	}
	time.Sleep(5 * time.Second)
	if _, err := t.containerID(dev, pr.Name, "running"); err != nil {
		return fmt.Errorf("after restore: %v", err)
	}
	return t.waitHTTP(fmt.Sprintf("http://%s:%d/", dev.addr(), port(4)), "", "", 200, 60*time.Second)
}

type serverBackups struct {
	Backups []struct {
		Name string `json:"name"`
		Size int64  `json:"size"`
	} `json:"backups"`
	Destinations []map[string]any `json:"destinations"`
}

func (t *T) newServerBackup() (name string, size int64, err error) {
	var b struct{ Name string }
	if err = t.api("POST", "/api/server-backups", nil, &b); err != nil {
		return
	}
	var v serverBackups
	if err = t.api("GET", "/api/server-backups", nil, &v); err != nil {
		return
	}
	for _, x := range v.Backups {
		if x.Name == b.Name {
			return x.Name, x.Size, nil
		}
	}
	return b.Name, 0, fmt.Errorf("backup %s created but not listed", b.Name)
}

func checkServerBackup(t *T) error {
	name, size, err := t.newServerBackup()
	if name != "" {
		t.cleanup(func() { t.c.call("DELETE", "/api/server-backups/"+url.PathEscape(name), nil, nil) })
	}
	if err != nil {
		return err
	}
	if size <= 0 {
		return fmt.Errorf("backup %s is empty", name)
	}
	t.logf("%s is %d bytes", name, size)
	return nil
}

func (t *T) healthUp() bool {
	_, err := t.c.call("GET", "/api/health", nil, nil)
	return err == nil
}

func checkServerRestore(t *T) error {
	var marker project
	dev, err := t.findDevice("LIVETEST_DEVICE")
	if err != nil {
		return err
	}
	marker0, err := t.newProject("marker", dev.ID, map[string]any{"type": "image", "image": "nginx:alpine"}, map[string]any{"restart": "no"})
	if err != nil {
		return err
	}
	marker = *marker0
	name, _, err := t.newServerBackup()
	if err != nil {
		return err
	}
	t.cleanup(func() { t.c.call("DELETE", "/api/server-backups/"+url.PathEscape(name), nil, nil) })
	if err := t.api("DELETE", "/api/projects/"+marker.ID, nil, nil); err != nil {
		return err
	}
	// Restoring replaces the server's state with the backup's, so changes made since the backup are lost.
	t.c.call("POST", "/api/server-backups/"+url.PathEscape(name)+"/restore", nil, nil)
	// The server restarts itself: wait for it to go away (or a grace period) and then come back.
	deadline := time.Now().Add(15 * time.Second)
	for t.healthUp() && time.Now().Before(deadline) {
		time.Sleep(time.Second)
	}
	if err := t.until("the server to come back", 3*time.Minute, func() (bool, error) { return t.healthUp(), nil }); err != nil {
		return err
	}
	var ps []project
	if err := t.api("GET", "/api/projects", nil, &ps); err != nil {
		return err
	}
	for _, p := range ps {
		if p.ID == marker.ID {
			return nil
		}
	}
	return fmt.Errorf("the server restarted but the project that only the backup knew is missing; the restore did not apply")
}

// withDestination adds a backup destination for the check and puts the old list back afterwards.
func (t *T) withDestination(dest map[string]any) (map[string]any, error) {
	var before serverBackups
	if err := t.api("GET", "/api/server-backups", nil, &before); err != nil {
		return nil, err
	}
	t.cleanup(func() { t.c.call("PUT", "/api/server-backups/destinations", before.Destinations, nil) })
	if err := t.api("PUT", "/api/server-backups/destinations", append(append([]map[string]any{}, before.Destinations...), dest), nil); err != nil {
		return nil, err
	}
	var after serverBackups
	if err := t.api("GET", "/api/server-backups", nil, &after); err != nil {
		return nil, err
	}
	for _, d := range after.Destinations {
		if d["name"] == dest["name"] {
			return d, nil
		}
	}
	return nil, fmt.Errorf("the destination was not saved")
}

// replicates makes a server backup and waits for the destination to report a copy without error.
func (t *T) replicates(destName string) error {
	name, _, err := t.newServerBackup()
	if name != "" {
		t.cleanup(func() { t.c.call("DELETE", "/api/server-backups/"+url.PathEscape(name), nil, nil) })
	}
	if err != nil {
		return err
	}
	return t.until("the copy to "+destName, 3*time.Minute, func() (bool, error) {
		var v serverBackups
		if err := t.api("GET", "/api/server-backups", nil, &v); err != nil {
			return false, err
		}
		for _, d := range v.Destinations {
			if d["name"] != destName {
				continue
			}
			if e, _ := d["last_error"].(string); e != "" {
				return false, fmt.Errorf("destination reported: %s", e)
			}
			if d["last_at"] != nil {
				return true, nil
			}
		}
		return false, nil
	})
}

func checkDestS3(t *T) error {
	d, err := t.withDestination(map[string]any{
		"kind": "s3", "name": "livetest-s3", "enabled": true,
		"endpoint": os.Getenv("LIVETEST_S3_ENDPOINT"), "bucket": os.Getenv("LIVETEST_S3_BUCKET"),
		"region": os.Getenv("LIVETEST_S3_REGION"), "prefix": "livetest/",
		"access_key": os.Getenv("LIVETEST_S3_ACCESS_KEY"), "secret_key": os.Getenv("LIVETEST_S3_SECRET_KEY"),
	})
	if err != nil {
		return err
	}
	if err := t.api("POST", fmt.Sprintf("/api/server-backups/destinations/%v/test", d["id"]), nil, nil); err != nil {
		return fmt.Errorf("connection test: %v", err)
	}
	return t.replicates("livetest-s3")
}

func checkDestDevice(t *T) error {
	dev, err := t.findDevice("LIVETEST_DEVICE2")
	if err != nil {
		return err
	}
	d, err := t.withDestination(map[string]any{"kind": "device", "name": "livetest-device", "enabled": true, "device_id": dev.ID, "path": "/tmp/livetest-backups"})
	if err != nil {
		return err
	}
	if err := t.api("POST", fmt.Sprintf("/api/server-backups/destinations/%v/test", d["id"]), nil, nil); err != nil {
		return fmt.Errorf("connection test: %v", err)
	}
	return t.replicates("livetest-device")
}

// withBaseDomain sets the base domain for the check and restores the old one afterwards.
func (t *T) withBaseDomain(dev *device) (string, error) {
	var s struct {
		BaseDomain string `json:"base_domain"`
	}
	if err := t.api("GET", "/api/settings", nil, &s); err != nil {
		return "", err
	}
	base := os.Getenv("LIVETEST_BASE_DOMAIN")
	t.cleanup(func() { t.c.call("PUT", "/api/settings/domain", map[string]any{"base_domain": s.BaseDomain}, nil) })
	if err := t.api("PUT", "/api/settings/domain", map[string]any{"base_domain": base}, nil); err != nil {
		return "", err
	}
	if err := t.api("POST", "/api/devices/"+dev.ID+"/proxy", nil, nil); err != nil {
		return "", fmt.Errorf("starting the proxy: %v", err)
	}
	if err := t.until("the Caddy proxy to run", 3*time.Minute, func() (bool, error) {
		d, err := t.device(dev.ID)
		return err == nil && d.Proxy == "running", err
	}); err != nil {
		return "", err
	}
	return base, nil
}

// viaProxy fetches https://host/ from the device's Caddy, without relying on DNS or a public certificate.
func viaProxy(ip, host, user, pass string) (int, string, error) {
	c := &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true, ServerName: host},
			Dial: func(_, _ string) (net.Conn, error) {
				return net.DialTimeout("tcp", net.JoinHostPort(ip, "443"), 10*time.Second)
			},
		}}
	req, _ := http.NewRequest("GET", "https://"+host+"/", nil)
	if user != "" {
		req.SetBasicAuth(user, pass)
	}
	resp, err := c.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, string(b), nil
}

func checkCaddy(t *T) error {
	dev, err := t.findDevice("LIVETEST_DEVICE")
	if err != nil {
		return err
	}
	pr, err := t.nginx(dev, "dom", port(5), nil)
	if err != nil {
		return err
	}
	base, err := t.withBaseDomain(dev)
	if err != nil {
		return err
	}
	host := pr.Name + "." + base
	// Plain HTTP on port 80 must at least be answered by Caddy (it redirects to HTTPS).
	if err := t.until("Caddy to answer on port 80 for "+host, 90*time.Second, func() (bool, error) {
		code, _, err := get("http://"+dev.addr()+"/", host, "", "")
		return err == nil && (code == 200 || code == 308 || code == 301), nil
	}); err != nil {
		return err
	}
	code, body, err := viaProxy(dev.addr(), host, "", "")
	if err != nil {
		return fmt.Errorf("HTTPS through Caddy failed (certificate issuance needs the name to resolve publicly, or use a name Caddy can issue for): %v", err)
	}
	if code != 200 || !strings.Contains(body, "nginx") {
		return fmt.Errorf("HTTPS through Caddy answered %d, want the nginx page", code)
	}
	return nil
}

func checkLoginGate(t *T) error {
	dev, err := t.findDevice("LIVETEST_DEVICE")
	if err != nil {
		return err
	}
	pr, err := t.nginx(dev, "gate", port(6), nil)
	if err != nil {
		return err
	}
	base, err := t.withBaseDomain(dev)
	if err != nil {
		return err
	}
	user, pass := "livetest-"+randHex(2), "Lt-"+randHex(8)
	var u struct{ ID string }
	if err := t.api("POST", "/api/users", map[string]any{"username": user, "password": pass, "role": "viewer"}, &u); err != nil {
		return fmt.Errorf("creating a test user: %v", err)
	}
	t.cleanup(func() { t.c.call("DELETE", "/api/users/"+u.ID, nil, nil) })
	if err := t.api("PUT", "/api/projects/"+pr.ID+"/domains/access", map[string]any{"protected": true}, nil); err != nil {
		return err
	}
	t.cleanup(func() {
		t.c.call("PUT", "/api/projects/"+pr.ID+"/domains/access", map[string]any{"protected": false}, nil)
	})
	// The control plane's verdict, as Caddy's forward_auth would ask for it.
	gate := t.c.base + "/api/gate/verify?project=" + pr.ID
	if code, _, err := get(gate, "", "", ""); err != nil || code != 401 {
		return fmt.Errorf("gate without credentials answered %d (%v), want 401", code, err)
	}
	if code, _, err := get(gate, "", user, "wrong"); err != nil || code != 401 {
		return fmt.Errorf("gate with a wrong password answered %d (%v), want 401", code, err)
	}
	if code, _, err := get(gate, "", user, pass); err != nil || code != 204 {
		return fmt.Errorf("gate with valid credentials answered %d (%v), want 204", code, err)
	}
	// And the real thing: Caddy in front of the container.
	host := pr.Name + "." + base
	var last string
	if err := t.until("Caddy to enforce the gate", 90*time.Second, func() (bool, error) {
		code, _, err := viaProxy(dev.addr(), host, "", "")
		if err != nil {
			last = err.Error()
			return false, nil
		}
		last = fmt.Sprint(code)
		return code == 401, nil
	}); err != nil {
		return fmt.Errorf("%v (last: %s)", err, last)
	}
	code, body, err := viaProxy(dev.addr(), host, user, pass)
	if err != nil || code != 200 || !strings.Contains(body, "nginx") {
		return fmt.Errorf("signed-in request through Caddy answered %d (%v), want the nginx page", code, err)
	}
	return nil
}

func checkEnvEncryption(t *T) error {
	dev, err := t.findDevice("LIVETEST_DEVICE")
	if err != nil {
		return err
	}
	secret := "livetest-secret-" + randHex(8)
	if _, err := t.newProject("env", dev.ID, map[string]any{"type": "image", "image": "nginx:alpine"}, map[string]any{"env": []string{"API_KEY=" + secret}, "restart": "no"}); err != nil {
		return err
	}
	path := os.Getenv("LIVETEST_DB_PATH")
	for _, f := range []string{path, path + "-wal"} {
		b, err := os.ReadFile(f)
		if err != nil {
			if f == path {
				return fmt.Errorf("reading the database file: %v (run this on the server's host, or mount the file)", err)
			}
			continue
		}
		if strings.Contains(string(b), secret) {
			return fmt.Errorf("the env value is stored in plain text in %s", f)
		}
	}
	return nil
}

func checkFreshRestore(t *T) error {
	fmt.Print(`
  Fresh-server restore of encrypted env values:
   1. Note a project that has a secret env value (or run env/encryption first).
   2. Make a server backup in the dashboard: Backups > Server > Back up now. Download it.
   3. Start a SECOND, empty server (new data volume) with the same ORCH_SECRET_KEY if you set one,
      or copy the <db>.key file if you did not.
   4. Restore the downloaded file there (Backups > Server > Restore from file).`)
	if err := t.ask("Do steps 1-4 now."); err != nil {
		return err
	}
	if err := t.confirm("After the restart, does the project show its env value decrypted (not blank, not garbage)?"); err != nil {
		return err
	}
	fmt.Print("\n  Now repeat the restore on a server WITHOUT the key to confirm it fails visibly rather than silently.\n")
	return t.confirm("Did the keyless server clearly refuse or show the values as unreadable?")
}

func checkAdopt(t *T) error {
	dev, err := t.findDevice("LIVETEST_DEVICE")
	if err != nil {
		return err
	}
	name := os.Getenv("LIVETEST_ADOPT_CONTAINER")
	id, state := dev.container(name)
	if id == "" {
		return fmt.Errorf("device %s has no container named %s", dev.Name, name)
	}
	body := func(dry bool) map[string]any {
		return map[string]any{"dry_run": dry, "containers": []map[string]any{{"id": id, "name": "livetest-adopt-" + randHex(2)}}}
	}
	if err := t.api("POST", "/api/devices/"+dev.ID+"/adopt", body(true), nil); err != nil {
		return fmt.Errorf("dry run: %v", err)
	}
	pname := body(false)["containers"].([]map[string]any)[0]["name"].(string)
	b := body(false)
	b["containers"].([]map[string]any)[0]["name"] = pname
	if err := t.api("POST", "/api/devices/"+dev.ID+"/adopt", b, nil); err != nil {
		return err
	}
	var ps []project
	t.api("GET", "/api/projects", nil, &ps)
	var p *project
	for i := range ps {
		if ps[i].Name == pname {
			p = &ps[i]
		}
	}
	if p == nil {
		return fmt.Errorf("adopted project %s was not created", pname)
	}
	// Do not delete the container with the project: it was the user's, and now Orchestrator's copy of it.
	t.cleanup(func() { t.c.call("DELETE", "/api/projects/"+p.ID, nil, nil) })
	if err := t.api("POST", "/api/projects/"+p.ID+"/deployments", nil, nil); err == nil {
		return fmt.Errorf("a first redeploy of an adopted container went ahead without confirmation")
	}
	var d deployment
	if err := t.api("POST", "/api/projects/"+p.ID+"/deployments?confirm=1", nil, &d); err != nil {
		return err
	}
	if _, err := t.waitDeployment(d.ID, 5*time.Minute); err != nil {
		return err
	}
	if _, err := t.containerID(dev, name, "running"); err != nil {
		return fmt.Errorf("the redeployed container is not running (it was %s before): %v", state, err)
	}
	return nil
}

func checkMigrate(t *T) error {
	from, err := t.findDevice("LIVETEST_DEVICE")
	if err != nil {
		return err
	}
	to, err := t.findDevice("LIVETEST_DEVICE2")
	if err != nil {
		return err
	}
	if from.ID == to.ID {
		return fmt.Errorf("LIVETEST_DEVICE and LIVETEST_DEVICE2 are the same device")
	}
	pr, err := t.nginx(from, "mig", port(7), map[string]any{"volumes": []string{"livetest-" + randHex(2) + "-mig:/usr/share/nginx/html"}})
	if err != nil {
		return err
	}
	if err := t.api("POST", "/api/projects/"+pr.ID+"/migrate", map[string]any{"device_id": to.ID, "with_data": true, "stop_source": true}, nil); err != nil {
		return err
	}
	if err := t.until("the migration", 10*time.Minute, func() (bool, error) {
		var m struct{ Status, Step, Error string }
		if err := t.api("GET", "/api/projects/"+pr.ID+"/migration", nil, &m); err != nil {
			return false, err
		}
		if m.Status == "failed" {
			return false, fmt.Errorf("migration failed at %s: %s", m.Step, m.Error)
		}
		return m.Status == "done", nil
	}); err != nil {
		return err
	}
	return t.waitHTTP(fmt.Sprintf("http://%s:%d/", to.addr(), port(7)), "", "", 200, 60*time.Second)
}

func checkTunnel(t *T) error {
	dev, err := t.findDevice("LIVETEST_DEVICE")
	if err != nil {
		return err
	}
	var tn struct {
		Supported bool     `json:"supported"`
		Enabled   bool     `json:"enabled"`
		State     string   `json:"state"`
		Warnings  []string `json:"warnings"`
	}
	if err := t.api("GET", "/api/devices/"+dev.ID+"/tunnel", nil, &tn); err != nil {
		return err
	}
	switch {
	case !tn.Supported:
		return fmt.Errorf("the agent on %s cannot run cloudflared; update it", dev.Name)
	case !tn.Enabled:
		return fmt.Errorf("no tunnel is set up on %s: add the tunnel token on the device's Settings page first", dev.Name)
	case tn.State != "running":
		return fmt.Errorf("the tunnel container is %q, want running (warnings: %v)", tn.State, tn.Warnings)
	}
	return nil
}

func checkSSO(t *T) error {
	var providers []struct{ ID, Name string }
	if _, err := t.c.call("GET", "/api/auth/sso/providers", nil, &providers); err != nil {
		return err
	}
	if len(providers) == 0 {
		return fmt.Errorf("no SSO provider is configured; set one up in Settings > Single sign-on first")
	}
	names := []string{}
	for _, p := range providers {
		names = append(names, p.Name)
	}
	if err := t.ask("Open %s in a private window and sign in with %s. Use an account on the allow-list.", t.c.base, strings.Join(names, " or ")); err != nil {
		return err
	}
	if err := t.confirm("Did you land in the dashboard with the right name and role?"); err != nil {
		return err
	}
	if err := t.ask("Sign out, then sign in with an account NOT on the allow-list."); err != nil {
		return err
	}
	return t.confirm("Was that second account refused?")
}

// ---- GitHub ----

func (t *T) ghProject(dev *device, suffix string, auto bool) (*project, error) {
	branch := os.Getenv("LIVETEST_GH_BRANCH")
	if branch == "" {
		branch = "main"
	}
	return t.newProject(suffix, dev.ID, map[string]any{
		"type": "github", "repo": os.Getenv("LIVETEST_GH_REPO"), "branch": branch,
		"context": os.Getenv("LIVETEST_GH_CONTEXT"), "auto_deploy": auto,
	}, map[string]any{"ports": []string{fmt.Sprintf("%d:80", port(8))}})
}

func (t *T) githubConnected() error {
	var s struct {
		GitHub struct {
			Connected bool `json:"connected"`
		} `json:"github"`
	}
	if err := t.api("GET", "/api/settings", nil, &s); err != nil {
		return err
	}
	if !s.GitHub.Connected {
		return fmt.Errorf("GitHub is not connected: add a token in Settings > GitHub first")
	}
	return nil
}

func latestSHA(repo, branch string) (string, error) {
	req, _ := http.NewRequest("GET", "https://api.github.com/repos/"+repo+"/commits/"+branch, nil)
	if tok := os.Getenv("LIVETEST_GH_TOKEN"); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var c struct {
		SHA    string `json:"sha"`
		Commit struct{ Message string }
	}
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("GitHub answered %d for %s@%s (private repo? set LIVETEST_GH_TOKEN)", resp.StatusCode, repo, branch)
	}
	return c.SHA, json.NewDecoder(resp.Body).Decode(&c)
}

func (t *T) deploymentIDs(projectID string) (map[string]bool, error) {
	var ds []deployment
	if err := t.api("GET", "/api/projects/"+projectID+"/deployments", nil, &ds); err != nil {
		return nil, err
	}
	m := map[string]bool{}
	for _, d := range ds {
		m[d.ID] = true
	}
	return m, nil
}

// newDeployment waits for a deployment that was not in before and returns it.
func (t *T) newDeployment(projectID string, before map[string]bool, limit time.Duration) (*deployment, error) {
	var found *deployment
	err := t.until("a new deployment", limit, func() (bool, error) {
		var ds []deployment
		if err := t.api("GET", "/api/projects/"+projectID+"/deployments", nil, &ds); err != nil {
			return false, err
		}
		for i := range ds {
			if !before[ds[i].ID] {
				found = &ds[i]
				return true, nil
			}
		}
		return false, nil
	})
	return found, err
}

func checkWebhookSigned(t *T) error {
	if err := t.githubConnected(); err != nil {
		return err
	}
	dev, err := t.findDevice("LIVETEST_DEVICE")
	if err != nil {
		return err
	}
	pr, err := t.ghProject(dev, "hook", true)
	if err != nil {
		return err
	}
	branch := os.Getenv("LIVETEST_GH_BRANCH")
	if branch == "" {
		branch = "main"
	}
	sha, err := latestSHA(os.Getenv("LIVETEST_GH_REPO"), branch)
	if err != nil {
		return err
	}
	var s struct {
		GitHub struct {
			Secret string `json:"webhook_secret"`
		} `json:"github"`
	}
	if err := t.api("GET", "/api/settings", nil, &s); err != nil {
		return err
	}
	payload, _ := json.Marshal(map[string]any{
		"ref": "refs/heads/" + branch, "after": sha, "repository": map[string]any{"full_name": os.Getenv("LIVETEST_GH_REPO")},
		"head_commit": map[string]any{"id": sha, "message": "livetest signed push", "author": map[string]any{"name": "livetest"}},
	})
	post := func(sig string) (int, error) {
		req, _ := http.NewRequest("POST", t.c.base+"/api/github/webhook", strings.NewReader(string(payload)))
		req.Header.Set("X-GitHub-Event", "push")
		req.Header.Set("X-Hub-Signature-256", sig)
		req.Header.Set("Content-Type", "application/json")
		resp, err := probe.Do(req)
		if err != nil {
			return 0, err
		}
		resp.Body.Close()
		return resp.StatusCode, nil
	}
	if code, err := post("sha256=" + strings.Repeat("0", 64)); err != nil || code != 401 {
		return fmt.Errorf("a forged signature answered %d (%v), want 401", code, err)
	}
	before, err := t.deploymentIDs(pr.ID)
	if err != nil {
		return err
	}
	mac := hmac.New(sha256.New, []byte(s.GitHub.Secret))
	mac.Write(payload)
	if code, err := post("sha256=" + hex.EncodeToString(mac.Sum(nil))); err != nil || code >= 300 {
		return fmt.Errorf("a correctly signed push answered %d (%v)", code, err)
	}
	d, err := t.newDeployment(pr.ID, before, 60*time.Second)
	if err != nil {
		return err
	}
	if d.Trigger != "push" {
		return fmt.Errorf("the new deployment's trigger is %q, want push", d.Trigger)
	}
	_, err = t.waitDeployment(d.ID, 10*time.Minute)
	return err
}

func checkRealPush(t *T) error {
	if err := t.githubConnected(); err != nil {
		return err
	}
	dev, err := t.findDevice("LIVETEST_DEVICE")
	if err != nil {
		return err
	}
	pr, err := t.ghProject(dev, "push", true)
	if err != nil {
		return err
	}
	var s struct {
		GitHub struct {
			Path string `json:"webhook_path"`
		} `json:"github"`
	}
	t.api("GET", "/api/settings", nil, &s)
	before, err := t.deploymentIDs(pr.ID)
	if err != nil {
		return err
	}
	if err := t.ask("Make sure the repo's webhook points at %s%s (content type JSON, push events). Then push any commit to the tracked branch of %s.", t.c.base, s.GitHub.Path, os.Getenv("LIVETEST_GH_REPO")); err != nil {
		return err
	}
	d, err := t.newDeployment(pr.ID, before, 90*time.Second)
	if err != nil {
		return fmt.Errorf("no deployment appeared after the push; check the webhook's Recent Deliveries on GitHub: %v", err)
	}
	if d.Trigger != "push" {
		return fmt.Errorf("the new deployment's trigger is %q, want push", d.Trigger)
	}
	_, err = t.waitDeployment(d.ID, 10*time.Minute)
	return err
}

func checkPreviewPR(t *T) error {
	if err := t.githubConnected(); err != nil {
		return err
	}
	dev, err := t.findDevice("LIVETEST_DEVICE")
	if err != nil {
		return err
	}
	pr, err := t.ghProject(dev, "pv", true)
	if err != nil {
		return err
	}
	if _, err := t.withBaseDomain(dev); err != nil {
		return err
	}
	if err := t.api("PUT", "/api/projects/"+pr.ID+"/previews/settings", map[string]any{"enabled": true, "max": 3}, nil); err != nil {
		return err
	}
	if err := t.ask("Open a pull request in %s from a branch of the same repo into the tracked branch.", os.Getenv("LIVETEST_GH_REPO")); err != nil {
		return err
	}
	type preview struct {
		ID       string `json:"id"`
		Slug     string `json:"slug"`
		HostPort int    `json:"host_port"`
	}
	var pv preview
	if err := t.until("a preview environment", 10*time.Minute, func() (bool, error) {
		var ps []preview
		if err := t.api("GET", "/api/projects/"+pr.ID+"/previews", nil, &ps); err != nil {
			return false, err
		}
		if len(ps) > 0 {
			pv = ps[0]
		}
		return len(ps) > 0, nil
	}); err != nil {
		return err
	}
	if err := t.waitHTTP(fmt.Sprintf("http://%s:%d/", dev.addr(), pv.HostPort), "", "", 200, 10*time.Minute); err != nil {
		return fmt.Errorf("preview container: %v", err)
	}
	if err := t.confirm(fmt.Sprintf("Does the preview open at its https URL (%s.%s)?", pv.Slug, os.Getenv("LIVETEST_BASE_DOMAIN"))); err != nil {
		return err
	}
	if err := t.ask("Close the pull request. If Cloudflare Tunnel hostnames are in use, also look at the DNS records for the preview hostname."); err != nil {
		return err
	}
	if err := t.until("the preview to be removed", 3*time.Minute, func() (bool, error) {
		var ps []preview
		if err := t.api("GET", "/api/projects/"+pr.ID+"/previews", nil, &ps); err != nil {
			return false, err
		}
		return len(ps) == 0, nil
	}); err != nil {
		return err
	}
	return t.confirm("Is the preview's tunnel CNAME gone from Cloudflare DNS (or was no tunnel used)?")
}

func checkNode(t *T) error {
	dev, err := t.findDevice("LIVETEST_DEVICE")
	if err != nil {
		return err
	}
	node := map[string]any{"static": os.Getenv("LIVETEST_NODE_STATIC") != ""}
	for k, v := range map[string]string{"install": "LIVETEST_NODE_INSTALL", "build": "LIVETEST_NODE_BUILD", "start": "LIVETEST_NODE_START", "version": "LIVETEST_NODE_VERSION", "output": "LIVETEST_NODE_OUTPUT"} {
		if os.Getenv(v) != "" {
			node[k] = os.Getenv(v)
		}
	}
	branch := os.Getenv("LIVETEST_NODE_BRANCH")
	if branch == "" {
		branch = "main"
	}
	hostPort, containerPort := port(9), "3000"
	if node["static"] == true {
		containerPort = "80"
	}
	pr, err := t.newProject("node", dev.ID, map[string]any{
		"type": "github", "repo": os.Getenv("LIVETEST_NODE_REPO"), "branch": branch,
		"context": os.Getenv("LIVETEST_NODE_CONTEXT"), "node": node,
	}, map[string]any{"ports": []string{fmt.Sprintf("%d:%s", hostPort, containerPort)}})
	if err != nil {
		return err
	}
	d, err := t.deployAndWait(pr, 20*time.Minute)
	if err != nil {
		return err
	}
	want := map[string]bool{"Cloning": false, "Installing": false, "Building": false}
	for _, s := range d.Steps {
		if _, ok := want[s.Name]; ok && s.Status == "succeeded" {
			want[s.Name] = true
		}
	}
	for k, ok := range want {
		if !ok {
			return fmt.Errorf("step %s did not succeed", k)
		}
	}
	return t.waitHTTP(fmt.Sprintf("http://%s:%d/", dev.addr(), hostPort), "", "", 200, 60*time.Second)
}

// ---- Cloudflare ----

func (t *T) cloudflareSettings() error {
	if err := t.api("PUT", "/api/settings/cloudflare/deploy", map[string]any{"account_id": os.Getenv("LIVETEST_CF_ACCOUNT_ID"), "token": os.Getenv("LIVETEST_CF_TOKEN")}, nil); err != nil {
		return fmt.Errorf("saving the Cloudflare account and token: %v", err)
	}
	return nil
}

func (t *T) cfProject(kind, repoVar, suffix string) (*project, error) {
	branch := os.Getenv("LIVETEST_CF_BRANCH")
	if branch == "" {
		branch = "main"
	}
	return t.newProject(suffix, "", map[string]any{
		"type": "cloudflare", "repo": os.Getenv(repoVar), "branch": branch, "auto_deploy": false,
		"context":    os.Getenv("LIVETEST_CF_CONTEXT"),
		"node":       map[string]any{"install": os.Getenv("LIVETEST_CF_INSTALL"), "build": os.Getenv("LIVETEST_CF_BUILD")},
		"cloudflare": map[string]any{"kind": kind, "output": os.Getenv("LIVETEST_CF_OUTPUT")},
	}, nil)
}

func checkCFPages(t *T) error {
	if err := t.cloudflareSettings(); err != nil {
		return err
	}
	pr, err := t.cfProject("pages", "LIVETEST_CF_PAGES_REPO", "pages")
	if err != nil {
		return err
	}
	d, err := t.deployAndWait(pr, 15*time.Minute)
	if err != nil {
		return err
	}
	if d.URL == "" {
		return fmt.Errorf("the deployment succeeded but has no URL")
	}
	t.logf("deployed to %s", d.URL)
	if err := t.waitHTTP(d.URL, "", "", 200, 3*time.Minute); err != nil {
		return err
	}
	var st struct{ State, Detail string }
	if err := t.api("POST", "/api/projects/"+pr.ID+"/cloudflare/refresh", nil, &st); err != nil {
		return err
	}
	if st.State == "" || st.State == "error" {
		return fmt.Errorf("Cloudflare monitoring reports %q: %s", st.State, st.Detail)
	}
	t.logf("Cloudflare says: %s %s", st.State, st.Detail)
	t.cleanup(func() {
		fmt.Printf("note  the Pages project %q stays in your Cloudflare account; delete it there\n", pr.Name)
	})
	return nil
}

func checkCFWorker(t *T) error {
	if err := t.cloudflareSettings(); err != nil {
		return err
	}
	pr, err := t.cfProject("worker", "LIVETEST_CF_WORKER_REPO", "worker")
	if err != nil {
		return err
	}
	d, err := t.deployAndWait(pr, 15*time.Minute)
	if err != nil {
		return err
	}
	t.cleanup(func() { fmt.Printf("note  the Worker %q stays in your Cloudflare account; delete it there\n", pr.Name) })
	target := os.Getenv("LIVETEST_CF_WORKER_URL")
	if target == "" {
		target = d.URL
	}
	if target != "" {
		for i := 0; i < 10; i++ {
			get(target, "", "", "")
		}
	}
	// Cloudflare's analytics lag by a minute or two, so give the query a few chances.
	var st struct {
		State, Detail, Analytics string
		Requests                 int64
	}
	err = t.until("Worker analytics to read back", 6*time.Minute, func() (bool, error) {
		if err := t.api("POST", "/api/projects/"+pr.ID+"/cloudflare/refresh", nil, &st); err != nil {
			return false, err
		}
		if st.Analytics != "ok" && st.Analytics != "none" && st.Analytics != "" && !strings.Contains(st.Analytics, "no data") {
			return false, fmt.Errorf("the analytics query failed: %s (this is the query that has never run live; the message shows what to fix)", st.Analytics)
		}
		return st.Analytics == "ok" && st.Requests > 0, nil
	})
	if err != nil {
		return fmt.Errorf("%v (analytics=%q requests=%d)", err, st.Analytics, st.Requests)
	}
	return nil
}

func checkCFDiscover(t *T) error {
	if err := t.cloudflareSettings(); err != nil {
		return err
	}
	var found json.RawMessage
	if err := t.api("GET", "/api/cloudflare/discover", nil, &found); err != nil {
		return err
	}
	t.logf("discover: %s", strings.TrimSpace(string(found)))
	return nil
}
