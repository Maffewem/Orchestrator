package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
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
	Started  time.Time `json:"started_at"`
	Finished time.Time `json:"finished_at,omitempty"`
	Log      []string  `json:"log"`
}

type migrations struct {
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
	withData := req.WithData == nil || *req.WithData
	if p.Source.Type == store.SourceCompose {
		writeError(w, http.StatusConflict, "stacks can't be migrated yet: redeploy the stack on the other device instead")
		return
	}
	if req.DeviceID == p.DeviceID {
		writeError(w, http.StatusBadRequest, "the project is already on that device")
		return
	}
	src, err := s.store.GetDevice(r.Context(), p.DeviceID)
	if err != nil {
		src = nil
		withData = false // the old device is gone: there is nothing to copy from
	}
	dst, err := s.store.GetDevice(r.Context(), req.DeviceID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "choose a device to move to")
		return
	}
	if dst.Maintenance {
		writeError(w, http.StatusConflict, errMaintenance(dst).Error())
		return
	}
	if dst.Offline {
		writeError(w, http.StatusConflict, dst.Name+" is offline")
		return
	}
	if withData && src != nil {
		if src.Offline {
			writeError(w, http.StatusConflict, src.Name+" is offline, so its data can't be copied. Bring it back online, or migrate without data")
			return
		}
		if findByName(src, serviceOf(p)) == nil {
			withData = false // never deployed: nothing to copy
		} else if !hasCap(src, api.CapBackup) {
			writeError(w, http.StatusConflict, errAgentTooOld(src, "copying data").Error())
			return
		}
	}
	if withData && !hasCap(dst, api.CapBackup) {
		writeError(w, http.StatusConflict, errAgentTooOld(dst, "copying data").Error())
		return
	}
	if p.Source.Type == store.SourceGitHub && !hasCap(dst, api.CapPreview) {
		writeError(w, http.StatusConflict, errAgentTooOld(dst, "migration").Error())
		return
	}

	s.mig.mu.Lock()
	if v := s.mig.byProject[p.ID]; v != nil && v.Status == migRunning {
		s.mig.mu.Unlock()
		writeError(w, http.StatusConflict, "a migration of this project is already running")
		return
	}
	if s.mig.byProject == nil {
		s.mig.byProject, s.mig.transfers = map[string]*MigrationView{}, map[string]string{}
	}
	from := "(deleted device)"
	if src != nil {
		from = src.Name
	}
	v := &MigrationView{Status: migRunning, Step: "Starting", From: from, To: dst.Name, Started: time.Now().UTC(), Log: []string{}}
	s.mig.byProject[p.ID] = v
	first := *v // the goroutine below mutates v, so the response gets a copy
	s.mig.mu.Unlock()

	s.event(r.Context(), dst.ID, "warning", "project.migrate", fmt.Sprintf("Moving %s from %s to %s", p.Name, from, dst.Name))
	go s.runMigration(context.WithoutCancel(r.Context()), p, src, dst, withData, req.StopSource)
	writeJSON(w, http.StatusAccepted, first)
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
		writeError(w, http.StatusNotFound, "no migration has run for this project since the server started")
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

func (s *Server) runMigration(ctx context.Context, p *store.Project, src, dst *store.Device, withData, stopSource bool) {
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
	if withData {
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
	if src != nil && stopSource {
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
