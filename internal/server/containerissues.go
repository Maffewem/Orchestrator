package server

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Maffewem/Orchestrator/internal/api"
	"github.com/Maffewem/Orchestrator/internal/store"
)

const settingContainerDismissals = "container_dismissals"

// A dismissal silences a container's alert. "ack" is somebody pressing Acknowledge: the alert stays
// quiet until the container runs again and fails again. "manual" is a stop the dashboard itself
// asked for (stop, Stop restarting, remove, a migration or failover), which is never an alert.
type dismissal struct {
	ContainerID string    `json:"container_id"`
	Reason      string    `json:"reason"`
	By          string    `json:"by,omitempty"`
	At          time.Time `json:"at"`
}

type dismissals struct {
	mu     sync.Mutex
	loaded bool
	m      map[string]dismissal // device|container name
}

// manualGrace is how long a container may still show as running after a requested stop before
// that counts as it having been started again.
const manualGrace = 3 * time.Minute

func dismissKey(deviceID, name string) string { return deviceID + "|" + name }

// load must be called with d.mu held.
func (s *Server) loadDismissals(ctx context.Context) {
	d := &s.dismissed
	if d.loaded {
		return
	}
	d.loaded = true
	d.m = map[string]dismissal{}
	if s.store == nil {
		return
	}
	s.getJSON(ctx, settingContainerDismissals, &d.m)
	if d.m == nil {
		d.m = map[string]dismissal{}
	}
}

func (s *Server) saveDismissals(ctx context.Context) {
	if s.store == nil {
		return
	}
	s.setJSON(ctx, settingContainerDismissals, s.dismissed.m)
}

// dismiss records a dismissal for the container, replacing any earlier one.
func (s *Server) dismiss(ctx context.Context, deviceID, containerID, name, reason, by string) {
	s.dismissed.mu.Lock()
	defer s.dismissed.mu.Unlock()
	s.loadDismissals(ctx)
	s.dismissed.m[dismissKey(deviceID, name)] = dismissal{ContainerID: containerID, Reason: reason, By: by, At: time.Now().UTC()}
	s.saveDismissals(ctx)
}

// markStopped notes that the dashboard itself is stopping the container, so its exit is expected.
func (s *Server) markStopped(ctx context.Context, deviceID string, c api.Container) {
	s.dismiss(ctx, deviceID, c.ID, c.Name, "manual", "")
}

// dismissedFor returns the container's current dismissal. A dismissal ends once the container has
// run again (so a new failure alerts afresh) or when it is a different container with the same name.
func (s *Server) dismissedFor(deviceID string, c api.Container) (dismissal, bool) {
	s.dismissed.mu.Lock()
	defer s.dismissed.mu.Unlock()
	ctx := context.Background()
	s.loadDismissals(ctx)
	key := dismissKey(deviceID, c.Name)
	dm, ok := s.dismissed.m[key]
	if !ok {
		return dm, false
	}
	stale := dm.ContainerID != "" && dm.ContainerID != c.ID
	if c.State == "running" && !c.Unstable {
		if dm.Reason == "manual" {
			stale = stale || time.Since(dm.At) > manualGrace
		} else {
			stale = true
		}
	}
	if stale {
		delete(s.dismissed.m, key)
		s.saveDismissals(ctx)
		return dm, false
	}
	return dm, true
}

// ContainerIssue is a failing container as the dashboard explains it: what happened, the numbers
// that show why, and what to do about it.
type ContainerIssue struct {
	Name string `json:"name"`
	ID   string `json:"id"`
	// Kind is oom (the kernel's out-of-memory killer), killed (exit 137 without proof of why),
	// loop (Docker keeps restarting it) or dead.
	Kind    string `json:"kind"`
	Message string `json:"message"`
	// Cause says in plain words what most likely happened, and Tips what to check next.
	Cause       string   `json:"cause"`
	Tips        []string `json:"tips,omitempty"`
	ProjectID   string   `json:"project_id,omitempty"`
	ProjectName string   `json:"project_name,omitempty"`
	// Memory figures in bytes. MemLimit is 0 when the container has no limit; MemUsed is the last
	// reading taken while it was running.
	MemLimit     uint64 `json:"mem_limit,omitempty"`
	MemUsed      uint64 `json:"mem_used,omitempty"`
	HostMemTotal uint64 `json:"host_mem_total,omitempty"`
	HostMemUsed  uint64 `json:"host_mem_used,omitempty"`
	SwapTotal    uint64 `json:"swap_total,omitempty"`
	SwapUsed     uint64 `json:"swap_used,omitempty"`
	// SuggestMB is a memory limit worth trying in the project settings; 0 when none is suggested.
	SuggestMB int `json:"suggest_mb,omitempty"`
	// SuggestWhy explains the suggested limit: which setting it is, what it is now, and why this value.
	SuggestWhy string `json:"suggest_why,omitempty"`
	// Acked is set when someone acknowledged it; it no longer counts towards the device's health.
	Acked    bool   `json:"acked,omitempty"`
	AckedBy  string `json:"acked_by,omitempty"`
	Restarts int    `json:"restarts,omitempty"`
}

const mib = 1 << 20

// suggestMemoryMB proposes the next limit to try for a container that was killed for memory:
// double a limit that was hit (or the peak use seen, when that was higher), rounded up to 256 MiB and
// never above 80% of the device's RAM. With no limit set there is nothing to raise (the container was
// already free to use all the RAM), so nothing is suggested.
func suggestMemoryMB(limit, used, hostTotal uint64) int {
	if limit == 0 {
		return 0
	}
	want := limit * 2
	if used > limit {
		want = used * 2
	}
	want = (want + 256*mib - 1) / (256 * mib) * (256 * mib)
	if hostTotal > 0 {
		if max := hostTotal / 10 * 8; want > max {
			want = max / (64 * mib) * (64 * mib)
		}
		if want <= limit {
			return 0 // the container already may use nearly all the device's RAM; a bigger limit won't help
		}
	}
	return int(want / mib)
}

// suggestWhy says exactly what setting a suggested limit refers to and where the number comes from.
func suggestWhy(mb int, limit, used, hostTotal, swapTotal uint64) string {
	if mb == 0 {
		if limit == 0 {
			return "This container has no memory limit, so it was free to use all of the device's RAM and the device itself ran out. A bigger limit would not help; free memory on the device (stop other containers, lower this one's worker counts) or add swap."
		}
		return fmt.Sprintf("The container's memory limit is already %s, close to all of the device's %s RAM, so raising it would not help. Free memory on the device, lower this container's own memory use, or add RAM.", memText(limit), memText(hostTotal))
	}
	why := fmt.Sprintf("This is the container's memory limit in the project settings (Docker's per-container cap), not the device's RAM or swap. It is %s now", memText(limit))
	if used > limit {
		why += fmt.Sprintf("; its peak use reached %s", memText(used))
	} else if used > 0 {
		why += fmt.Sprintf("; its last reading was %s", memText(used))
	}
	why += fmt.Sprintf(". %d MiB is double that so the same load fits with headroom", mb)
	if hostTotal > 0 {
		why += fmt.Sprintf(", and stays under 80%% of the device's %s RAM", memText(hostTotal))
	}
	why += "."
	if swapTotal == 0 {
		why += " The device has no swap, so a limit above its free RAM will still be killed; swap is separate and is set on the device itself."
	}
	return why
}

func memText(b uint64) string {
	if b >= 1<<30 {
		return fmt.Sprintf("%.1f GiB", float64(b)/(1<<30))
	}
	return fmt.Sprintf("%d MiB", b/mib)
}

// describeContainerIssue builds the explanation for one failing container.
func (s *Server) describeContainerIssue(ctx context.Context, d *store.Device, c api.Container, kind string, projects []*store.Project) ContainerIssue {
	m := d.Metrics
	is := ContainerIssue{Name: c.Name, ID: c.ID, Kind: kind, HostMemTotal: m.MemoryTotal, HostMemUsed: m.MemoryUsed, SwapTotal: m.SwapTotal, SwapUsed: m.SwapUsed}
	if p := containerOwner(projects, d, c); p != nil {
		is.ProjectID, is.ProjectName = p.ID, p.Name
	}
	if s.wstats != nil {
		s.wstats.mu.Lock()
		if acc := s.wstats.workers[d.ID+"|"+c.Name]; acc != nil {
			is.MemLimit, is.MemUsed, is.Restarts = acc.latest.MemLimit, acc.latest.MemUsed, acc.latest.Restarts
			if acc.memMax > is.MemUsed {
				is.MemUsed = acc.memMax
			}
		}
		s.wstats.mu.Unlock()
	}
	hostLow := m.MemoryTotal > 0 && float64(m.MemoryUsed)/float64(m.MemoryTotal) >= 0.85
	host := ""
	if m.MemoryTotal > 0 {
		host = fmt.Sprintf("The device is using %s of %s RAM", memText(m.MemoryUsed), memText(m.MemoryTotal))
		if m.SwapTotal > 0 {
			host += fmt.Sprintf(" and %s of %s swap", memText(m.SwapUsed), memText(m.SwapTotal))
		} else {
			host += " and has no swap"
		}
		host += " right now."
	}
	switch kind {
	case "oom":
		if is.MemLimit > 0 {
			is.Cause = fmt.Sprintf("Linux killed it for running out of memory. It had a %s memory limit and hit it (last reading %s).", memText(is.MemLimit), memText(is.MemUsed))
		} else {
			is.Cause = "Linux killed it for running out of memory. It has no memory limit, so the whole device ran out of RAM."
		}
	case "killed":
		if c.ExitInspected {
			is.Cause = "It stopped with exit code 137 (SIGKILL). Docker did not mark it as killed for running out of memory, and its log shows no stop request, so this is probably not a memory problem. Something outside Docker sent it SIGKILL (for example kill -9 or a script). It is listed for information and does not count as a device warning."
		} else {
			is.Cause = "It was killed (exit code 137, which is SIGKILL). The usual reason is Linux's out-of-memory killer, but this device's agent is too old to tell that apart from a container stopped by hand (a stop that times out also ends in 137). Update the agent for a definite answer."
		}
		if hostLow && !c.ExitInspected {
			is.Cause += " The device is low on memory, which makes an out-of-memory kill likely."
		}
	case "loop":
		is.Cause = "Docker keeps restarting it, so it starts and then fails again within seconds."
	default:
		is.Cause = "Docker marked it dead, which means it could not be stopped or removed cleanly."
	}
	if (kind == "oom" || (kind == "killed" && !c.ExitInspected)) && host != "" {
		is.Cause += " " + host
	}
	if kind == "oom" || (kind == "killed" && !c.ExitInspected) {
		is.SuggestMB = suggestMemoryMB(is.MemLimit, is.MemUsed, m.MemoryTotal)
		is.SuggestWhy = suggestWhy(is.SuggestMB, is.MemLimit, is.MemUsed, m.MemoryTotal, m.SwapTotal)
		is.Tips = []string{
			"Check the container's logs for the last lines before it stopped.",
			"On the device, `docker stats` shows live memory use per container, and `free -h` shows RAM and swap.",
		}
		if m.SwapTotal == 0 {
			is.Tips = append(is.Tips, "The device has no swap. A small swap file gives a brief spike somewhere to go instead of being killed.")
		}
		if strings.Contains(strings.ToLower(c.Image+" "+c.Name), "paperless") {
			is.Tips = append(is.Tips, "Paperless-ngx uses most of its memory in OCR workers. Lower PAPERLESS_TASK_WORKERS and PAPERLESS_THREADS_PER_WORKER (for example 1 and 1) in its environment settings.")
		}
	}
	switch kind {
	case "oom":
		is.Message = "Containers killed for running out of memory: " + c.Name
	case "killed":
		is.Message = "Containers killed (exit 137): " + c.Name
	case "loop":
		is.Message = "Containers restarting repeatedly: " + c.Name
	default:
		is.Message = "Containers dead: " + c.Name
	}
	return is
}

func (s *Server) handleAckContainerIssue(w http.ResponseWriter, r *http.Request) {
	d, ok := s.lookupDevice(w, r)
	if !ok {
		return
	}
	var req struct {
		Container string `json:"container"` // name
		Undo      bool   `json:"undo"`
	}
	if !decode(w, r, &req) {
		return
	}
	var found *api.Container
	for i := range d.Containers {
		if d.Containers[i].Name == req.Container {
			found = &d.Containers[i]
		}
	}
	if found == nil {
		writeError(w, http.StatusBadRequest, "container is not known on this device")
		return
	}
	who := "someone"
	if id := identityFrom(r); id != nil && id.Name != "" {
		who = id.Name
	}
	if req.Undo {
		s.dismissed.mu.Lock()
		s.loadDismissals(r.Context())
		delete(s.dismissed.m, dismissKey(d.ID, found.Name))
		s.saveDismissals(r.Context())
		s.dismissed.mu.Unlock()
	} else {
		s.dismiss(r.Context(), d.ID, found.ID, found.Name, "ack", who)
		s.event(r.Context(), d.ID, "info", "container.alert_ack", fmt.Sprintf("%s: %s acknowledged the alert for %s", d.Name, who, found.Name))
	}
	writeJSON(w, http.StatusOK, s.view(d))
}
