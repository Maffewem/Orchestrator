package agent

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"
)

// Backup format: a gzip-compressed tar holding manifest.json (the container's inspect
// output plus the list of backed-up mounts) and, for every writable volume or bind mount,
// that mount's files under mounts/<index>/. The image is not included: it is pulled or
// rebuilt by a deployment.

const manifestName = "manifest.json"

type backupMount struct {
	Index       int    `json:"index"`
	Type        string `json:"type"`
	Name        string `json:"name,omitempty"`
	Source      string `json:"source,omitempty"`
	Destination string `json:"destination"`
}

type backupManifest struct {
	Version   int             `json:"version"`
	Name      string          `json:"name"`
	CreatedAt time.Time       `json:"created_at"`
	Mounts    []backupMount   `json:"mounts"`
	Container json.RawMessage `json:"container"`
}

// InspectRaw returns the container's full inspect document.
func (d *Docker) InspectRaw(ctx context.Context, id string) (json.RawMessage, error) {
	var raw json.RawMessage
	err := d.call(ctx, http.MethodGet, "/containers/"+id+"/json", nil, nil, &raw)
	return raw, err
}

// ArchiveGet streams the tar of a path inside a container.
func (d *Docker) ArchiveGet(ctx context.Context, id, p string) (io.ReadCloser, error) {
	resp, err := d.request(ctx, http.MethodGet, "/containers/"+id+"/archive", url.Values{"path": {p}}, nil)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// ArchivePut extracts a tar into the directory dir inside a container.
func (d *Docker) ArchivePut(ctx context.Context, id, dir string, body io.Reader) error {
	resp, err := d.do(ctx, http.MethodPut, "/containers/"+id+"/archive", url.Values{"path": {dir}}, body, "application/x-tar")
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// backupMounts picks the mounts worth saving: writable volumes and bind mounts, but not
// sockets or the host's pseudo-filesystems.
func backupMounts(inspect json.RawMessage) ([]backupMount, string, error) {
	var v struct {
		Name   string `json:"Name"`
		Mounts []struct {
			Type, Name, Source, Destination string
			RW                              bool
		} `json:"Mounts"`
	}
	if err := json.Unmarshal(inspect, &v); err != nil {
		return nil, "", err
	}
	var out []backupMount
	for _, m := range v.Mounts {
		if (m.Type != "volume" && m.Type != "bind") || !m.RW || strings.HasSuffix(m.Destination, ".sock") {
			continue
		}
		if m.Type == "bind" && (m.Source == "/proc" || m.Source == "/sys" || m.Source == "/dev" || strings.HasPrefix(m.Source, "/proc/") || strings.HasPrefix(m.Source, "/sys/") || strings.HasPrefix(m.Source, "/dev/")) {
			continue
		}
		if m.Destination == "/" || m.Destination == "" {
			continue
		}
		out = append(out, backupMount{Index: len(out), Type: m.Type, Name: m.Name, Source: m.Source, Destination: m.Destination})
	}
	return out, strings.TrimPrefix(v.Name, "/"), nil
}

// backup builds the archive and streams it to the control plane as it goes, so nothing is
// staged on the device's (often small) SD card.
func (a *Agent) backup(ctx context.Context, containerID, backupID string) error {
	inspect, err := a.docker.InspectRaw(ctx, containerID)
	if err != nil {
		return err
	}
	mounts, name, err := backupMounts(inspect)
	if err != nil {
		return err
	}

	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(a.writeArchive(ctx, pw, containerID, name, inspect, mounts)) }()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.cfg.Server+"/api/agent/backups/"+url.PathEscape(backupID), pr)
	if err != nil {
		pr.Close()
		return err
	}
	req.Header.Set("Authorization", "Bearer "+a.state.AgentToken)
	req.Header.Set("Content-Type", "application/gzip")
	req.Header.Set("User-Agent", "orchestrator-agent/"+Version)
	resp, err := (&http.Client{}).Do(req) // no overall timeout: large volumes on slow links
	if err != nil {
		pr.CloseWithError(err)
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		pr.CloseWithError(errors.New("upload rejected"))
		return fmt.Errorf("control plane rejected the backup (HTTP %d): %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return nil
}

func (a *Agent) writeArchive(ctx context.Context, w io.Writer, containerID, name string, inspect json.RawMessage, mounts []backupMount) error {
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)

	man, err := json.MarshalIndent(backupManifest{Version: 1, Name: name, CreatedAt: time.Now().UTC(), Mounts: mounts, Container: inspect}, "", "  ")
	if err != nil {
		return err
	}
	if err := tw.WriteHeader(&tar.Header{Name: manifestName, Mode: 0o600, Size: int64(len(man)), ModTime: time.Now()}); err != nil {
		return err
	}
	if _, err := tw.Write(man); err != nil {
		return err
	}

	for _, m := range mounts {
		body, err := a.docker.ArchiveGet(ctx, containerID, m.Destination)
		if isStatus(err, http.StatusNotFound) {
			a.log.Warn("backup: mount path missing, skipped", "path", m.Destination)
			continue
		} else if err != nil {
			return fmt.Errorf("read %s: %w", m.Destination, err)
		}
		err = copyPrefixed(tw, body, "mounts/"+strconv.Itoa(m.Index)+"/")
		body.Close()
		if err != nil {
			return fmt.Errorf("archive %s: %w", m.Destination, err)
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

// copyPrefixed re-emits every entry of src under prefix.
func copyPrefixed(tw *tar.Writer, src io.Reader, prefix string) error {
	tr := tar.NewReader(src)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		} else if err != nil {
			return err
		}
		h.Name = prefix + h.Name
		if h.Typeflag == tar.TypeLink {
			h.Linkname = prefix + h.Linkname
		}
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		if _, err := io.Copy(tw, tr); err != nil {
			return err
		}
	}
}

// restoreBackup writes a backup's mount data back into an existing container. The container is
// stopped while files are written and started again if it was running. Files in the backup
// overwrite current ones; files created since the backup are left alone.
func (a *Agent) restoreBackup(ctx context.Context, containerID, backupID string) (err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.cfg.Server+"/api/agent/backups/"+url.PathEscape(backupID), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+a.state.AgentToken)
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("could not download the backup (HTTP %d)", resp.StatusCode)
	}
	gz, err := gzip.NewReader(resp.Body)
	if err != nil {
		return fmt.Errorf("backup is not a valid archive: %w", err)
	}
	tr := tar.NewReader(gz)

	h, err := tr.Next()
	if err != nil || h.Name != manifestName {
		return errors.New("backup has no manifest")
	}
	var man backupManifest
	if err := json.NewDecoder(tr).Decode(&man); err != nil {
		return fmt.Errorf("bad manifest: %w", err)
	}
	dest := map[int]string{}
	for _, m := range man.Mounts {
		dest[m.Index] = m.Destination
	}

	state, _, err := a.docker.Inspect(ctx, containerID)
	if err != nil {
		return err
	}
	if state.Running {
		if err := a.docker.Stop(ctx, containerID); err != nil {
			return fmt.Errorf("stop container: %w", err)
		}
		defer func() {
			if serr := a.docker.Start(ctx, containerID); serr != nil && err == nil {
				err = fmt.Errorf("start container: %w", serr)
			}
		}()
	}

	var (
		cur  = -1
		tw   *tar.Writer
		pw   *io.PipeWriter
		done chan error
	)
	finish := func(werr error) error {
		if pw == nil {
			return werr
		}
		if werr == nil {
			werr = tw.Close()
		}
		pw.CloseWithError(werr) // nil closes cleanly
		perr := <-done
		pw, tw = nil, nil
		if werr != nil {
			return werr
		}
		return perr
	}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		} else if err != nil {
			return finish(err)
		}
		rest, ok := strings.CutPrefix(h.Name, "mounts/")
		idxStr, rest, ok2 := strings.Cut(rest, "/")
		idx, convErr := strconv.Atoi(idxStr)
		if !ok || !ok2 || convErr != nil || dest[idx] == "" || rest == "" {
			continue
		}
		if idx != cur {
			if err := finish(nil); err != nil {
				return err
			}
			cur = idx
			var pr *io.PipeReader
			pr, pw = io.Pipe()
			tw = tar.NewWriter(pw)
			done = make(chan error, 1)
			dir, mount := path.Dir(dest[idx]), dest[idx]
			go func() {
				perr := a.docker.ArchivePut(ctx, containerID, dir, pr)
				if perr != nil {
					perr = fmt.Errorf("restore %s: %w", mount, perr)
				}
				pr.CloseWithError(perr) // unblock the writer if Docker gave up early
				done <- perr
			}()
		}
		if !safeEntry(rest) || (h.Typeflag == tar.TypeLink && !safeEntry(strings.TrimPrefix(h.Linkname, "mounts/"+strconv.Itoa(idx)+"/"))) {
			return finish(fmt.Errorf("backup contains an unsafe path %q", h.Name))
		}
		h.Name = rest
		if h.Typeflag == tar.TypeLink {
			h.Linkname = strings.TrimPrefix(strings.TrimPrefix(h.Linkname, "mounts/"+strconv.Itoa(idx)+"/"), "/")
		}
		if err := tw.WriteHeader(h); err != nil {
			return finish(err)
		}
		if _, err := io.Copy(tw, tr); err != nil {
			return finish(err)
		}
	}
	return finish(nil)
}

// safeEntry reports whether a tar entry path stays inside the directory it is extracted to.
func safeEntry(p string) bool {
	if strings.HasPrefix(p, "/") {
		return false
	}
	for _, part := range strings.Split(p, "/") {
		if part == ".." {
			return false
		}
	}
	return true
}
