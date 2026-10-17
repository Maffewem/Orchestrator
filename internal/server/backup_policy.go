package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
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
}

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
	return nil
}

type backupPolicyView struct {
	BackupPolicy
	NextRun *time.Time `json:"next_run,omitempty"`
	LastRun *time.Time `json:"last_run,omitempty"`
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
	ids, err := s.store.PruneScheduledBackups(ctx, deviceID, name, pol.Keep)
	if err != nil {
		s.log.Error("prune backups", "err", err)
	}
	for _, id := range ids {
		os.Remove(s.backupPath(id))
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
			s.backupTick(ctx, time.Now())
		}
	}
}

// backupTick starts the scheduled backups that have come due.
func (s *Server) backupTick(ctx context.Context, now time.Time) {
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
