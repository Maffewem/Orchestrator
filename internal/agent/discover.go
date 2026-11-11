package agent

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/Maffewem/Orchestrator/internal/api"
)

// When the control plane moves to a new address (new DHCP lease, Wi-Fi to ethernet) an agent
// has no one to ask where it went. So after every known address has failed, the agent goes
// looking: it tries this machine, then every host on its own local networks, on the port it
// already knows, and asks each one to prove it is the same server by signing a fresh nonce
// with the identity key the agent saved while it was connected. The agent only ever dials
// out; nothing here listens.

// discoverEvery spaces out searches so a long outage does not turn into a constant scan.
var discoverEvery = 2 * time.Minute

// maybeDiscover runs after a failed heartbeat and reports whether it moved the agent to a
// newly found address.
func (a *Agent) maybeDiscover(ctx context.Context) bool {
	a.fails++
	a.srvMu.Lock()
	known := len(a.servers)
	a.srvMu.Unlock()
	if a.state.ServerKey == "" || a.fails < max(3, known) || time.Since(a.lastDiscover) < discoverEvery {
		return false
	}
	a.lastDiscover = time.Now()
	a.log.Info("no known address answers; looking for the control plane on the local network")
	found := a.discover(ctx)
	if found == "" {
		a.log.Warn("control plane not found on the local network; will look again later")
		return false
	}
	a.log.Info("found the control plane at a new address", "server", found)
	a.state.Found = found
	a.saveState()
	a.setServers(found)
	return true
}

// setServers rebuilds the address list: configured ones first, then those the server
// announced, then the last discovered one. current, when set, becomes the address in use.
func (a *Agent) setServers(current string) {
	a.srvMu.Lock()
	defer a.srvMu.Unlock()
	seen := map[string]bool{}
	var all []string
	for _, u := range append(append(append([]string{}, a.configured...), a.state.Servers...), a.state.Found) {
		if u = strings.TrimRight(strings.TrimSpace(u), "/"); u != "" && !seen[u] {
			seen[u] = true
			all = append(all, u)
		}
	}
	a.servers = all
	if current != "" {
		a.cfg.Server = current
	}
}

// learn saves what a successful heartbeat said about the server: its identity key and the
// addresses the admin listed for agents.
func (a *Agent) learn(res api.HeartbeatResponse) {
	a.fails = 0
	changed := false
	if res.ServerKey != "" && res.ServerKey != a.state.ServerKey {
		a.state.ServerKey = res.ServerKey
		changed = true
	}
	if res.ServerKey != "" && strings.Join(res.ServerURLs, "\n") != strings.Join(a.state.Servers, "\n") {
		a.state.Servers = res.ServerURLs
		changed = true
	}
	if a.state.Server != a.server() {
		a.state.Server = a.server()
		changed = true
	}
	if changed {
		a.saveState()
		a.setServers("")
	}
}

func (a *Agent) saveState() {
	b, _ := json.Marshal(a.state)
	if err := os.WriteFile(a.cfg.StatePath, b, 0o600); err != nil {
		a.log.Warn("save state", "err", err)
	}
}

// discover returns the base URL of the first host that proves it is this agent's server.
func (a *Agent) discover(ctx context.Context) string {
	key, err := base64.StdEncoding.DecodeString(a.state.ServerKey)
	if err != nil || len(key) != ed25519.PublicKeySize {
		return ""
	}
	// The port is the one thing that survives an address change. Plain HTTP only: an HTTPS
	// certificate names a host, so a bare IP would not pass verification anyway.
	a.srvMu.Lock()
	var ports []string
	seen := map[string]bool{}
	for _, s := range a.servers {
		u, err := url.Parse(s)
		if err != nil || u.Scheme != "http" {
			continue
		}
		p := u.Port()
		if p == "" {
			p = "80"
		}
		if !seen[p] {
			seen[p] = true
			ports = append(ports, p)
		}
	}
	a.srvMu.Unlock()
	if len(ports) == 0 {
		return ""
	}
	hosts := []string{"127.0.0.1"}
	if a.scanHosts != nil {
		hosts = append(hosts, a.scanHosts()...)
	} else {
		hosts = append(hosts, localHosts()...)
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	client := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{
		DialContext:       (&net.Dialer{Timeout: 700 * time.Millisecond}).DialContext,
		DisableKeepAlives: true,
	}}
	jobs := make(chan string)
	found := make(chan string, 1)
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for base := range jobs {
				if identify(ctx, client, base, key) {
					select {
					case found <- base:
						cancel()
					default:
					}
				}
			}
		}()
	}
feed:
	for _, h := range hosts {
		for _, p := range ports {
			select {
			case jobs <- "http://" + net.JoinHostPort(h, p):
			case <-ctx.Done():
				break feed
			}
		}
	}
	close(jobs)
	wg.Wait()
	select {
	case base := <-found:
		return base
	default:
		return ""
	}
}

// identify asks base to sign a fresh nonce and checks the signature against key.
func identify(ctx context.Context, client *http.Client, base string, key ed25519.PublicKey) bool {
	nonce := make([]byte, 24)
	if _, err := rand.Read(nonce); err != nil {
		return false
	}
	n := hex.EncodeToString(nonce)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/agent/identify?nonce="+n, nil)
	if err != nil {
		return false
	}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	var res api.IdentifyResponse
	if json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 4096)).Decode(&res) != nil {
		return false
	}
	sig, err := base64.StdEncoding.DecodeString(res.Signature)
	return err == nil && ed25519.Verify(key, api.IdentifyMessage(n), sig)
}

// localHosts lists the other addresses on this machine's own IPv4 networks. Container
// bridges are skipped, and a network larger than 1022 hosts is narrowed to the /24 around
// this machine so a scan stays small.
func localHosts() []string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 || virtualIface(ifc.Name) {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, ad := range addrs {
			ipn, ok := ad.(*net.IPNet)
			if !ok {
				continue
			}
			out = append(out, hostsIn(ipn, seen)...)
		}
	}
	return out
}

func virtualIface(name string) bool {
	for _, p := range []string{"docker", "br-", "veth", "virbr", "cni", "flannel", "cali", "lxc"} {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

func hostsIn(ipn *net.IPNet, seen map[string]bool) []string {
	ip := ipn.IP.To4()
	if ip == nil {
		return nil
	}
	ones, _ := ipn.Mask.Size()
	if ones < 22 {
		ones = 24
	}
	if ones > 30 {
		return nil
	}
	mask := net.CIDRMask(ones, 32)
	base := uint32(ip[0])<<24 | uint32(ip[1])<<16 | uint32(ip[2])<<8 | uint32(ip[3])
	m := uint32(mask[0])<<24 | uint32(mask[1])<<16 | uint32(mask[2])<<8 | uint32(mask[3])
	network, broadcast := base&m, base|^m
	var out []string
	for v := network + 1; v < broadcast; v++ {
		h := net.IPv4(byte(v>>24), byte(v>>16), byte(v>>8), byte(v)).String()
		if !seen[h] {
			seen[h] = true
			out = append(out, h)
		}
	}
	return out
}
