package server

import (
	"net"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/Maffewem/Orchestrator/internal/store"
)

// Alert is one thing that needs someone's attention, shown under the top-bar bell. Alerts are
// derived from current state on every request, so each one clears itself once the problem is fixed.
type Alert struct {
	ID       string    `json:"id"`
	Severity string    `json:"severity"` // critical | warning | info
	Title    string    `json:"title"`
	Detail   string    `json:"detail,omitempty"`
	Link     string    `json:"link"`
	At       time.Time `json:"at,omitempty"`
}

var severityRank = map[string]int{"critical": 0, "warning": 1, "info": 2}

func (s *Server) handleAlerts(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	out := []Alert{}

	// A dashboard reached over plain HTTP sends sign-in tokens in the clear. Localhost is exempt.
	if !requestIsHTTPS(r) && !isLoopbackHost(r.Host) {
		out = append(out, Alert{ID: "security:http", Severity: "warning", Title: "The dashboard is served over plain HTTP",
			Detail: "Anyone on the network path can read sign-in tokens and, unless commands are signed, tamper with what devices are told to run. Turn on HTTPS in Settings, or put the dashboard behind an HTTPS proxy or tunnel.",
			Link:   "/settings", At: time.Now()})
	}

	// Devices: one alert per device, with all of its issues combined.
	devices, err := s.store.ListDevices(ctx)
	if err != nil {
		s.internal(w, err)
		return
	}
	names := map[string]string{}
	for _, d := range devices {
		names[d.ID] = d.Name
		v := s.view(d)
		if v.Health == "ok" {
			continue
		}
		title := d.Name + " needs attention"
		if v.Status == "offline" {
			title = d.Name + " is offline"
		}
		out = append(out, Alert{ID: "device:" + d.ID, Severity: v.Health, Title: title, Detail: strings.Join(v.Issues, "; "), Link: "/devices/" + d.ID, At: d.LastSeen})
	}

	// Projects whose latest production deployment failed.
	projects, err := s.store.ListProjects(ctx)
	if err != nil {
		s.internal(w, err)
		return
	}
	for _, p := range projects {
		// A replica's outcome is reported separately below, so it can't hide the main copy's.
		for _, rs := range s.replicaStates(ctx, p, s.clusterPolicy(ctx, p.ID)) {
			if rs.DeploymentStatus == string(store.DeploymentFailed) && !rs.Running {
				out = append(out, Alert{ID: "replica:" + rs.DeploymentID, Severity: "warning", Title: "Replica failed: " + p.Name + " · " + rs.Device + " (replica)", Detail: rs.DeploymentError, Link: "/cluster", At: time.Now()})
			}
		}
		ds, err := s.store.ListDeployments(ctx, store.DeploymentFilter{ProjectID: p.ID, DeviceID: p.DeviceID, Limit: 1})
		if err != nil || len(ds) == 0 || ds[0].Status != store.DeploymentFailed {
			continue
		}
		d := ds[0]
		detail := d.Error
		if d.Commit != nil && d.Commit.SHA != "" {
			detail = strings.TrimSpace("Commit " + shortSHA(d.Commit.SHA) + ". " + detail)
		}
		title := "Deployment failed: " + p.Name
		if len(s.clusterPolicy(ctx, p.ID).Replicas) > 0 {
			title += " · " + names[p.DeviceID] + " (main)"
		}
		out = append(out, Alert{ID: "deploy:" + d.ID, Severity: "critical", Title: title, Detail: detail, Link: "/deployments/" + d.ID, At: d.CreatedAt})
	}

	// Containers whose most recent backup failed.
	backups, err := s.store.ListBackups(ctx, "")
	if err == nil {
		seen := map[string]bool{}
		for _, b := range backups { // newest first
			key := b.DeviceID + "/" + b.ContainerName
			if seen[key] {
				continue
			}
			seen[key] = true
			if b.Status == store.BackupFailed {
				out = append(out, Alert{ID: "backup:" + b.ID, Severity: "warning", Title: "Backup failed: " + b.ContainerName, Detail: strings.TrimSpace(b.DeviceName + ". " + b.Error), Link: "/backups", At: b.CreatedAt})
			}
		}
	}

	// Backups taking most of the disk they are stored on.
	if id := identityFrom(r); id != nil && id.Role == store.RoleAdmin {
		if u := s.backupUsageWithLocation(ctx); u.Level != "ok" {
			out = append(out, Alert{ID: "backup-storage", Severity: map[string]string{"critical": "critical", "warning": "warning"}[u.Level], Title: "Backups are using most of the disk",
				Detail: humanSize(u.BackupBytes) + " of backups in " + u.where() + ", " + humanSize(u.FreeBytes) + " free (" + humanSize(u.ContainerBytes) + " worker backups, " + humanSize(u.ServerBytes) + " server backups). Delete old backups, lower how many are kept, or move them to a larger disk.", Link: "/backups"})
		}
	}

	// A newer server version, for the people who can install it.
	if id := identityFrom(r); id != nil && id.Role == store.RoleAdmin {
		if chk, ok := s.storedCheck(ctx); ok && chk.Newer {
			out = append(out, Alert{ID: "update:" + chk.Version, Severity: "info", Title: "Update available: " + chk.Version, Detail: "Open Updates to install it.", Link: "/updates", At: chk.CheckedAt})
		}
	}

	sort.SliceStable(out, func(i, j int) bool {
		if a, b := severityRank[out[i].Severity], severityRank[out[j].Severity]; a != b {
			return a < b
		}
		return out[i].At.After(out[j].At)
	})
	writeJSON(w, http.StatusOK, out)
}

func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

func isLoopbackHost(hostport string) bool {
	h, _, err := net.SplitHostPort(hostport)
	if err != nil {
		h = hostport
	}
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(strings.Trim(h, "[]"))
	return ip != nil && ip.IsLoopback()
}
