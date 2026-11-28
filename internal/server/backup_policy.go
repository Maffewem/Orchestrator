package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/Maffewem/Orchestrator/internal/api"
	"github.com/Maffewem/Orchestrator/internal/store"
)

const defaultKeep = 7

// BackupPolicy is a worker's automatic backup schedule.
type BackupPolicy struct {
	Mode    string `json:"mode"` // off | daily | weekly
	Time    string `json:"time"`
	Weekday int    `json:"weekday"`
	TZ      string `json:"tz"`
	Keep    int    `json:"keep"` // scheduled backups to keep
	// Consolidate (daily mode) keeps the newest Keep daily backups and thins older ones to a
	// single backup per week, keeping WeeklyKeep weeks.
	Consolidate bool `json:"consolidate"`
	WeeklyKeep  int  `json:"weekly_keep"`
}

const defaultWeeklyKeep = 4

func defaultBackupPolicy() BackupPolicy {
	return BackupPolicy{Mode: "off", Time: "02:00", TZ: "UTC", Keep: defaultKeep}
}

func (p BackupPolicy) schedule() Schedule {
	return Schedule{Mode: p.Mode, Time: p.Time, Weekday: p.Weekday, TZ: p.TZ}
}

func (p BackupPolicy) validate() error {
	if err := p.schedule().validate(); err != nil {
		return err
	}
	if p.Keep < 1 || p.Keep > 100 {
		return errors.New("keep must be between 1 and 100")
	}
	if p.Consolidate && p.WeeklyKeep != 0 && (p.WeeklyKeep < 1 || p.WeeklyKeep > 52) {
		return errors.New("weekly backups to keep must be between 1 and 52")
	}
	return nil
}

type backupPolicyView struct {
	BackupPolicy
	NextRun *time.Time `json:"next_run,omitempty"`
	LastRun *time.Time `json:"last_run,omitempty"`
	// EstimatedSize is the expected size of one backup, taken from this worker's recent backups
	// (0 when it has none). EstimatedTotal is that size times the backups kept under the policy.
	EstimatedSize  int64 `json:"estimated_size"`
	EstimatedTotal int64 `json:"estimated_total"`
}

// retained is the largest number of scheduled backups the policy keeps at once.
func (p BackupPolicy) retained() int {
	n := p.Keep
	if p.Mode == "daily" && p.Consolidate {
		n += p.weeklyKeep()
	}
	return n
}

func (p BackupPolicy) weeklyKeep() int {
	if p.WeeklyKeep < 1 {
		return defaultWeeklyKeep
	}
	return p.WeeklyKeep
}

// estimateBackup predicts the size of the next backup of a worker as the largest of its three
// most recent completed backups. Compressed size varies little between backups of the same data.
func (s *Server) estimateBackup(ctx context.Context, deviceID, name string) (int64, int) {
	all, err := s.store.ListBackups(ctx, deviceID)
	if err != nil {
		return 0, 0
	}
	var est int64
	n := 0
	for _, b := range all { // newest first
		if b.ContainerName != name || b.Status != store.BackupComplete || b.Size <= 0 {
			continue
		}
		if b.Size > est {
			est = b.Size
		}
		if n++; n == 3 {
			break
		}
	}
	return est, n
}

// consolidationVictims picks the scheduled backups (newest first) to delete: beyond the newest
// keep, only the newest backup of each calendar week (UTC, Monday start) survives, for weeks not
// already covered by a kept daily backup, and only weeklyKeep of those weeks.
func consolidationVictims(list []*store.Backup, keep, weeklyKeep int) []string {
	week := func(t time.Time) string {
		y, w := t.UTC().ISOWeek()
		return fmt.Sprintf("%d-%02d", y, w)
	}
	covered := map[string]bool{}
	var victims []string
	for i, b := range list {
		if i < keep {
			covered[week(b.CreatedAt)] = true
			continue
		}
		wk := week(b.CreatedAt)
		if covered[wk] {
			victims = append(victims, b.ID)
			continue
		}
		covered[wk] = true
		if weeklyKeep--; weeklyKeep < 0 {
			victims = append(victims, b.ID)
		}
	}
	return victims
}

func (s *Server) loadBackupPolicy(ctx context.Context, deviceID, name string) (BackupPolicy, *store.BackupPolicy) {
	pol := defaultBackupPolicy()
	row, err := s.store.GetBackupPolicy(ctx, deviceID, name)
	if err != nil {
		return pol, nil
	}
	json.Unmarshal([]byte(row.Policy), &pol)
	return pol, row
}

func (s *Server) policyView(ctx context.Context, deviceID, name string) backupPolicyView {
	pol, row := s.loadBackupPolicy(ctx, deviceID, name)
	v := backupPolicyView{BackupPolicy: pol}
	if est, _ := s.estimateBackup(ctx, deviceID, name); est > 0 {
		v.EstimatedSize = est
		v.EstimatedTotal = est * int64(pol.retained())
	}
	if next, ok := pol.schedule().nextSlot(time.Now()); ok {
		n := next.UTC()
		v.NextRun = &n
	}
	if row != nil && row.LastRun.Unix() > 0 {
		l := row.LastRun
		v.LastRun = &l
	}
	return v
}

func (s *Server) handleGetBackupPolicy(w http.ResponseWriter, r *http.Request) {
	d, ok := s.lookupDevice(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, s.policyView(r.Context(), d.ID, r.PathValue("name")))
}

func (s *Server) handleSetBackupPolicy(w http.ResponseWriter, r *http.Request) {
	d, ok := s.lookupDevice(w, r)
	if !ok {
		return
	}
	name := r.PathValue("name")
	var pol BackupPolicy
	if !decode(w, r, &pol) {
		return
	}
	if err := pol.validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	known := false
	for _, c := range d.Containers {
		known = known || c.Name == name
	}
	if !known {
		writeError(w, http.StatusBadRequest, "no worker with that name on this device")
		return
	}
	if pol.Mode != "daily" {
		pol.Consolidate = false
	}
	if pol.Consolidate && pol.WeeklyKeep == 0 {
		pol.WeeklyKeep = defaultWeeklyKeep
	}
	if pol.Mode != "off" && !hasCap(d, api.CapBackup) {
		writeError(w, http.StatusConflict, errAgentTooOld(d, "backups").Error())
		return
	}
	b, _ := json.Marshal(pol)
	if err := s.store.SetBackupPolicy(r.Context(), d.ID, name, string(b)); err != nil {
		s.internal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.policyView(r.Context(), d.ID, name))
}

// pruneBackups applies a worker's retention to its scheduled backups.
func (s *Server) pruneBackups(ctx context.Context, deviceID, name string) {
	pol, _ := s.loadBackupPolicy(ctx, deviceID, name)
	var ids []string
	if pol.Mode == "daily" && pol.Consolidate {
		all, err := s.store.ListBackups(ctx, deviceID)
		if err != nil {
			s.log.Error("prune backups", "err", err)
			return
		}
		var sched []*store.Backup
		for _, b := range all { // newest first
			if b.ContainerName == name && b.Source == "scheduled" && b.Status == store.BackupComplete {
				sched = append(sched, b)
			}
		}
		for _, id := range consolidationVictims(sched, pol.Keep, pol.weeklyKeep()) {
			if err := s.store.DeleteBackup(ctx, id); err != nil {
				s.log.Error("prune backups", "err", err)
				continue
			}
			ids = append(ids, id)
		}
	} else {
		var err error
		if ids, err = s.store.PruneScheduledBackups(ctx, deviceID, name, pol.Keep); err != nil {
			s.log.Error("prune backups", "err", err)
		}
	}
	for _, id := range ids {
		s.removeBackupFiles(id)
	}
}

func (s *Server) backupLoop(ctx context.Context) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if !s.movePaused() {
				s.backupTick(ctx, time.Now())
			}
		}
	}
}

// backupTick starts the scheduled backups that have come due.
func (s *Server) backupTick(ctx context.Context, now time.Time) {
	s.serverBackupTick(ctx, now)
	s.dockerGCTick(ctx, now)
	rows, err := s.store.ListBackupPolicies(ctx)
	if err != nil {
		s.log.Error("backup policies", "err", err)
		return
	}
	for _, row := range rows {
		var pol BackupPolicy
		if json.Unmarshal([]byte(row.Policy), &pol) != nil || pol.Mode == "off" {
			continue
		}
		baseline := row.SetAt
		if row.LastRun.After(baseline) {
			baseline = row.LastRun
		}
		if !pol.schedule().due(baseline, now) {
			continue
		}
		d, err := s.store.GetDevice(ctx, row.DeviceID)
		if err != nil || s.view(d).Status != "online" {
			continue // try again when the device is back
		}
		var c *api.Container
		for i := range d.Containers {
			if d.Containers[i].Name == row.ContainerName {
				c = &d.Containers[i]
			}
		}
		if c == nil {
			continue
		}
		s.store.MarkBackupPolicyRun(ctx, d.ID, row.ContainerName, now)
		if _, err := s.createBackup(ctx, d, c, "scheduled"); err != nil {
			s.event(ctx, d.ID, "error", "backup.failed", fmt.Sprintf("Scheduled backup of %s not started: %v", c.Name, err))
		}
	}
}
