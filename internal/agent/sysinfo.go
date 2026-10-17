package agent

import (
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
	return h
}
