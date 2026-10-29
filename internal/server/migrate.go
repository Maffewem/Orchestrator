package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Maffewem/Orchestrator/internal/api"
	"github.com/Maffewem/Orchestrator/internal/store"
)

// Migration moves a project to another device: back up its data, deploy it on the target,
// copy the data into the new container, then optionally stop the old one. The project only
// points at the new device once the backup is safely on the server, and goes back to the old
// one if the deployment fails, so a failed migration leaves the source untouched.

const (
	migRunning = "running"
	migDone    = "done"
	migFailed  = "failed"
	// sourceMigration tags backups made for a migration.
	sourceMigration = "migration"
)

// MigrationView is the progress of the latest migration of a project.
type MigrationView struct {
	Status   string    `json:"status"`
	Step     string    `json:"step"`
	Error    string    `json:"error,omitempty"`
	From     string    `json:"from"`
	To       string    `json:"to"`
	FromID   string    `json:"from_id,omitempty"`
	ToID     string    `json:"to_id,omitempty"`
	Started  time.Time `json:"started_at"`
	Finished time.Time `json:"finished_at,omitempty"`
	Log      []string  `json:"log"`
}

type migrations struct {
	// persist saves progress so it survives a server restart (see recoverMigrations).
	persist   func(project string, v MigrationView)
	mu        sync.Mutex
	byProject map[string]*MigrationView
	// transfers lets the target device download a backup that came from another device.
	transfers map[string]string // backup id -> target device id
}

func (m *migrations) update(project string, f func(v *MigrationView)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if v := m.byProject[project]; v != nil {
		f(v)
		if m.persist != nil {
			c := *v
			c.Log = append([]string(nil), v.Log...)
			m.persist(project, c)
		}
	}
}

func (m *migrations) step(project, step string) {
	m.update(project, func(v *MigrationView) {
		v.Step = step
		v.Log = append(v.Log, time.Now().UTC().Format("15:04:05")+" "+step)
	})
}

func (m *migrations) mayDownload(backupID, deviceID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.transfers[backupID] == deviceID
}

type migrateRequest struct {
	DeviceID string `json:"device_id"`
	// WithData copies the container's volumes and bind-mount contents (default true).
	WithData *bool `json:"with_data"`
	// StopSource stops the old container once the new one is verified.
	StopSource bool `json:"stop_source"`
	// FromBackup restores the newest backup on the server instead of copying live data. It is for
	// a source device that is offline.
	FromBackup bool `json:"from_backup"`
}

func (s *Server) handleMigrateProject(w http.ResponseWriter, r *http.Request) {
	p, ok := s.lookupProject(w, r)
	if !ok {
		return
	}
	var req migrateRequest
	if !decode(w, r, &req) {
		return
	}
	first, status, err := s.startMigration(r.Context(), p, req)
	if err != nil {
		writeError(w, status, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, first)
}

// startMigration checks a move and starts it in the background. On failure it returns the HTTP status to answer with.
func (s *Server) startMigration(ctx context.Context, p *store.Project, req migrateRequest) (MigrationView, int, error) {
	var none MigrationView
	withData := req.WithData == nil || *req.WithData
	isStack := p.Source.Type == store.SourceCompose
	if isStack {
		withData = false // volumes of a stack are not copied
	}
	if req.DeviceID == p.DeviceID {
		return none, http.StatusBadRequest, errors.New("the project is already on that device")
	}
	src, err := s.store.GetDevice(ctx, p.DeviceID)
	if err != nil {
		src = nil
		withData = false // the old device is gone: there is nothing to copy from
	}
	dst, err := s.store.GetDevice(ctx, req.DeviceID)
	if err != nil {
		return none, http.StatusBadRequest, errors.New("choose a device to move to")
	}
	if dst.Maintenance {
		return none, http.StatusConflict, errMaintenance(dst)
	}
	if dst.Offline {
		return none, http.StatusConflict, errors.New(dst.Name + " is offline")
	}
	var useBackup *store.Backup
	if withData && src != nil {
		switch {
		case src.Offline && req.FromBackup:
			bs, _ := s.store.ListBackups(ctx, src.ID)
			for _, b := range bs { // newest first
				if b.ContainerName == serviceOf(p) && b.Status == store.BackupComplete {
					useBackup = b
					break
				}
			}
			if useBackup == nil {
				return none, http.StatusConflict, errors.New(src.Name + " is offline and the server has no backup of " + p.Name + " to restore from")
			}
		case src.Offline:
			return none, http.StatusConflict, errors.New(src.Name + " is offline, so its data can't be copied. Bring it back online, move from the newest backup on the server (the data may be out of date), or migrate without data")
		case findByName(src, serviceOf(p)) == nil:
			withData = false // never deployed: nothing to copy
		case !hasCap(src, api.CapBackup):
			return none, http.StatusConflict, errAgentTooOld(src, "copying data")
		}
	}
	if withData && !hasCap(dst, api.CapBackup) {
		return none, http.StatusConflict, errAgentTooOld(dst, "copying data")
	}
	if isStack && !hasCap(dst, api.CapCompose) {
		return none, http.StatusConflict, errAgentTooOld(dst, "stacks")
	}
	if p.Source.Type == store.SourceGitHub && !hasCap(dst, api.CapPreview) {
		return none, http.StatusConflict, errAgentTooOld(dst, "migration")
	}

	s.mig.mu.Lock()
	if v := s.mig.byProject[p.ID]; v != nil && v.Status == migRunning {
		s.mig.mu.Unlock()
		return none, http.StatusConflict, errors.New("a migration of this project is already running")
	}
	if s.mig.byProject == nil {
		s.mig.byProject, s.mig.transfers = map[string]*MigrationView{}, map[string]string{}
	}
	from, fromID := "(deleted device)", ""
	if src != nil {
		from, fromID = src.Name, src.ID
	}
	v := &MigrationView{Status: migRunning, Step: "Starting", From: from, To: dst.Name, FromID: fromID, ToID: dst.ID, Started: time.Now().UTC(), Log: []string{}}
	s.mig.byProject[p.ID] = v
	first := *v // the goroutine below mutates v, so the response gets a copy
	if s.mig.persist != nil {
		s.mig.persist(p.ID, first)
	}
	s.mig.mu.Unlock()

	s.event(ctx, dst.ID, "warning", "project.migrate", fmt.Sprintf("Moving %s from %s to %s", p.Name, from, dst.Name))
	go s.runMigration(context.WithoutCancel(ctx), p, src, dst, withData, req.StopSource, useBackup)
	return first, http.StatusAccepted, nil
}

const migrationKey = "migration:"

func (s *Server) persistMigration(project string, v MigrationView) {
	b, _ := json.Marshal(v)
	if err := s.store.SetSetting(context.Background(), migrationKey+project, string(b)); err != nil {
		s.log.Error("save migration", "err", err)
	}
}

// recoverMigrations runs at boot. A migration that was in progress when the server stopped can't
// carry on, so it is marked failed and the project is left on whichever device actually runs it.
func (s *Server) recoverMigrations(ctx context.Context) {
	saved, err := s.store.ListSettingsWithPrefix(ctx, migrationKey)
	if err != nil {
		s.log.Error("recover migrations", "err", err)
		return
	}
	for key, raw := range saved {
		var v MigrationView
		if json.Unmarshal([]byte(raw), &v) != nil || v.Status != migRunning {
			continue
		}
		id := strings.TrimPrefix(key, migrationKey)
		v.Status, v.Finished = migFailed, time.Now().UTC()
		msg := "The server restarted during the move. Nothing was changed."
		if p, err := s.store.GetProject(ctx, id); err == nil && v.ToID != "" && p.DeviceID == v.ToID {
			dst, derr := s.store.GetDevice(ctx, v.ToID)
			if derr == nil && findByName(dst, serviceOf(p)) != nil {
				msg = fmt.Sprintf("The server restarted during the move. %s is now on %s: check that its data arrived (a migration backup is on the Backups page) and stop the old container yourself if it is still running.", p.Name, v.To)
			} else if v.FromID != "" {
				p.DeviceID = v.FromID
				s.store.UpdateProject(ctx, p)
				s.syncProxy(ctx, v.ToID)
				s.syncProxy(ctx, v.FromID)
				msg = fmt.Sprintf("The server restarted during the move before %s ran on %s, so it was put back on %s.", p.Name, v.To, v.From)
			}
		}
		v.Error = msg
		v.Log = append(v.Log, time.Now().UTC().Format("15:04:05")+" Failed: "+msg)
		s.persistMigration(id, v)
		s.log.Warn("migration interrupted", "project", id)
	}
}

func (s *Server) handleGetMigration(w http.ResponseWriter, r *http.Request) {
	s.mig.mu.Lock()
	v := s.mig.byProject[r.PathValue("id")]
	var cp *MigrationView
	if v != nil {
		c := *v
		c.Log = append([]string(nil), v.Log...)
		cp = &c
	}
	s.mig.mu.Unlock()
	if cp == nil {
		// Not run since this process started: show the last saved state, if any.
		if raw, err := s.store.GetSetting(r.Context(), migrationKey+r.PathValue("id")); err == nil {
			var saved MigrationView
			if json.Unmarshal([]byte(raw), &saved) == nil {
				cp = &saved
			}
		}
	}
	if cp == nil {
		writeError(w, http.StatusNotFound, "no migration has run for this project")
		return
	}
	writeJSON(w, http.StatusOK, cp)
}

func findByName(d *store.Device, name string) *api.Container {
	for i := range d.Containers {
		if d.Containers[i].Name == name {
			return &d.Containers[i]
		}
	}
	return nil
}

var errMigrateTimeout = errors.New("timed out")

// waitFor polls cond until it reports done, fails, or the timeout passes.
func (s *Server) waitFor(ctx context.Context, timeout time.Duration, cond func() (bool, error)) error {
	poll := s.migratePoll
	if poll == 0 {
		poll = 2 * time.Second
	}
	deadline := time.Now().Add(timeout)
	for {
		done, err := cond()
		if err != nil || done {
			return err
		}
		if time.Now().After(deadline) {
			return errMigrateTimeout
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(poll):
		}
	}
}

func (s *Server) runMigration(ctx context.Context, p *store.Project, src, dst *store.Device, withData, stopSource bool, useBackup *store.Backup) {
	m := &s.mig
	fail := func(err error) {
		m.update(p.ID, func(v *MigrationView) {
			v.Status, v.Error, v.Finished = migFailed, err.Error(), time.Now().UTC()
			v.Log = append(v.Log, time.Now().UTC().Format("15:04:05")+" Failed: "+err.Error())
		})
		s.event(ctx, dst.ID, "error", "project.migrate", fmt.Sprintf("Moving %s to %s failed: %s", p.Name, dst.Name, err))
	}
	name := serviceOf(p)

	// 1. Back up the data from the old device.
	var backup *store.Backup
	if useBackup != nil {
		backup = useBackup
		m.step(p.ID, fmt.Sprintf("%s is offline: using the backup from %s", src.Name, useBackup.CreatedAt.Format("2006-01-02 15:04")))
		m.mu.Lock()
		m.transfers[backup.ID] = dst.ID
		m.mu.Unlock()
		defer func() {
			m.mu.Lock()
			delete(m.transfers, backup.ID)
			m.mu.Unlock()
		}()
	} else if withData {
		m.step(p.ID, "Backing up "+name+" on "+src.Name)
		c := findByName(src, name)
		b, err := s.createBackup(ctx, src, c, sourceMigration)
		if err != nil {
			fail(err)
			return
		}
		backup = b
		err = s.waitFor(ctx, 2*time.Hour, func() (bool, error) {
			cur, err := s.store.GetBackup(ctx, b.ID)
			if err != nil {
				return false, err
			}
			if cur.Status == store.BackupFailed {
				return false, fmt.Errorf("backup failed: %s", cur.Error)
			}
			return cur.Status == store.BackupComplete, nil
		})
		if err != nil {
			fail(fmt.Errorf("backing up %s: %w", name, err))
			return
		}
		m.mu.Lock()
		m.transfers[b.ID] = dst.ID
		m.mu.Unlock()
		defer func() {
			m.mu.Lock()
			delete(m.transfers, b.ID)
			m.mu.Unlock()
		}()
	}

	// 2. Point the project at the new device and deploy there.
	m.step(p.ID, "Deploying to "+dst.Name)
	oldDevice := p.DeviceID
	p.DeviceID = dst.ID
	if err := s.store.UpdateProject(ctx, p); err != nil {
		fail(err)
		return
	}
	revert := func() {
		p.DeviceID = oldDevice
		s.store.UpdateProject(ctx, p)
		s.syncProxy(ctx, dst.ID)
		s.syncProxy(ctx, oldDevice)
	}
	var commit *store.Commit
	if p.Source.FromRepo() {
		var err error
		if commit, err = s.latestCommit(ctx, p.Source.Repo, p.Source.Branch); err != nil {
			revert()
			fail(err)
			return
		}
	}
	dep, err := s.deploy(ctx, p, commit, "migration")
	if err != nil {
		revert()
		fail(err)
		return
	}
	err = s.waitFor(ctx, 45*time.Minute, func() (bool, error) {
		cur, err := s.store.GetDeployment(ctx, dep.ID)
		if err != nil {
			return false, err
		}
		if cur.Status == store.DeploymentFailed {
			return false, fmt.Errorf("deployment failed: %s", cur.Error)
		}
		return cur.Status == store.DeploymentSucceeded, nil
	})
	if err != nil {
		revert()
		fail(fmt.Errorf("%w. %s was left on %s", err, p.Name, src.Name))
		return
	}

	// 3. Copy the data into the new container.
	if backup != nil {
		m.step(p.ID, "Copying data into the new container")
		var target string
		err := s.waitFor(ctx, 2*time.Minute, func() (bool, error) {
			d, err := s.store.GetDevice(ctx, dst.ID)
			if err != nil {
				return false, err
			}
			if c := findByName(d, name); c != nil {
				target = c.ID
			}
			return target != "", nil
		})
		if err == nil {
			var cmd *api.Command
			if cmd, err = s.store.CreateCommand(ctx, dst.ID, api.ActionRestore, target, map[string]string{"backup_id": backup.ID}); err == nil {
				err = s.waitFor(ctx, 2*time.Hour, func() (bool, error) {
					cur, err := s.store.GetCommand(ctx, cmd.ID)
					if err != nil {
						return false, err
					}
					if cur.Status == api.CommandFailed {
						return false, errors.New(cur.Error)
					}
					return cur.Status == api.CommandSucceeded, nil
				})
			}
		}
		if err != nil {
			// The new container is running without its data; the old one is untouched.
			fail(fmt.Errorf("copying data: %w. %s now runs on %s without its data; the backup is kept on the Backups page and can be restored there", err, p.Name, dst.Name))
			s.syncProxy(ctx, oldDevice)
			s.syncProxy(ctx, dst.ID)
			return
		}
	}

	// 4. Domains follow the project, then the old copy is retired.
	s.syncProxy(ctx, dst.ID)
	s.syncProxy(ctx, oldDevice)
	for _, line := range s.moveTunnelRoutes(ctx, p, oldDevice, dst) {
		m.step(p.ID, line)
	}
	s.logDomainMoves(ctx, p, dst)
	if src != nil && stopSource && !src.Offline && p.Source.Type == store.SourceCompose {
		m.step(p.ID, "Stopping the old stack on "+src.Name)
		if _, err := s.store.CreateCommand(ctx, src.ID, api.ActionStackStop, p.Name, nil); err != nil {
			s.log.Error("migrate: stop source stack", "err", err)
		}
	} else if src != nil && stopSource && !src.Offline {
		if c := findByName(src, name); c != nil && hasCap(src, api.CapPreview) {
			m.step(p.ID, "Stopping the old container on "+src.Name)
			if _, err := s.store.CreateCommand(ctx, src.ID, api.ActionStop, c.ID, nil); err != nil {
				s.log.Error("migrate: stop source", "err", err)
			}
		}
	}
	m.update(p.ID, func(v *MigrationView) {
		v.Status, v.Step, v.Finished = migDone, "Done", time.Now().UTC()
		v.Log = append(v.Log, time.Now().UTC().Format("15:04:05")+" Done")
	})
	s.event(ctx, dst.ID, "info", "project.migrate", fmt.Sprintf("%s moved to %s", p.Name, dst.Name))
}

// logDomainMoves tells the migration log what happened to the project's domains: with Cloudflare
// connected the A records were already updated by the proxy sync, otherwise they must be changed by hand.
func (s *Server) logDomainMoves(ctx context.Context, p *store.Project, dst *store.Device) {
	var hosts []string
	if base := s.baseDomain(ctx); base != "" && s.urlPrefs(ctx, p.ID).Production {
		hosts = append(hosts, slug(p.Name)+"."+base)
	}
	if list, err := s.store.ListDomains(ctx, p.ID); err == nil {
		for _, dm := range list {
			hosts = append(hosts, dm.Hostname)
		}
	}
	if len(hosts) == 0 {
		return
	}
	ips := ipv4s(dst)
	if s.cloudflareToken(ctx) != "" {
		s.mig.step(p.ID, "Cloudflare DNS for "+strings.Join(hosts, ", ")+" now points at "+dst.Name)
		return
	}
	s.mig.step(p.ID, "Update DNS for "+strings.Join(hosts, ", ")+" to point at "+strings.Join(ips, ", ")+" ("+dst.Name+")")
}
