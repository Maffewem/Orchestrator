package agent

import (
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/Maffewem/Orchestrator/internal/api"
)

// hostPath resolves a host file, honouring HostRoot when the agent runs in a container
// with the host filesystem mounted (e.g. -v /:/host:ro).
func (a *Agent) hostPath(p string) string {
	if a.cfg.HostRoot == "" {
		return p
	}
	return filepath.Join(a.cfg.HostRoot, p)
}

func readTrim(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(strings.TrimRight(string(b), "\x00"))
}

// osRelease returns PRETTY_NAME from os-release, e.g. "Debian GNU/Linux 12 (bookworm)".
func osRelease(path string) string {
	for _, line := range strings.Split(readTrim(path), "\n") {
		if v, ok := strings.CutPrefix(line, "PRETTY_NAME="); ok {
			return strings.Trim(v, `"`)
		}
	}
	return ""
}

func (a *Agent) hostInfo() api.HostInfo {
	h := api.HostInfo{
		OS:           runtime.GOOS,
		Arch:         runtime.GOARCH,
		CPUs:         runtime.NumCPU(),
		AgentVersion: Version,
		InContainer:  InContainer(),
		Capabilities: []string{api.CapBackup, api.CapUpdate, api.CapProxy, api.CapProxyDNS, api.CapPreview, api.CapStream, api.CapInspect, api.CapFileStore},
		Server:       strings.TrimRight(a.cfg.Server, "/"),
	}
	if a.cfg.HostRoot != "" {
		h.Hostname = readTrim(a.hostPath("/etc/hostname"))
	}
	if h.Hostname == "" {
		h.Hostname, _ = os.Hostname()
	}
	if runtime.GOOS == "linux" {
		h.Platform = osRelease(a.hostPath("/etc/os-release"))
		h.Kernel = readTrim("/proc/sys/kernel/osrelease")
		// Raspberry Pis (and most ARM boards) name themselves here.
		h.Model = readTrim("/proc/device-tree/model")
		h.MemoryTotal = meminfo()["MemTotal"]
	}
	h.Addresses = addresses()
	return h
}

// addresses lists the device's non-loopback IPv4 addresses (host networking only, when
// the agent runs in a container).
func addresses() []string {
	var out []string
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 ||
			strings.HasPrefix(ifc.Name, "docker") || strings.HasPrefix(ifc.Name, "br-") || strings.HasPrefix(ifc.Name, "veth") {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil {
				out = append(out, ipn.IP.String())
			}
		}
	}
	return out
}
