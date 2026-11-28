package server

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/Maffewem/Orchestrator/internal/api"
)

// Gold images: raw disk images (optionally gz, xz or zst compressed) stored next to the database.
// An admin uploads one; an admin can then ask a device to write it onto a spare disk. The agent
// downloads it with its own token and verifies the checksum recorded here.

const maxImageBytes = 128 << 30

var imageIDRe = regexp.MustCompile(`^img_[a-f0-9]{16}$`)

func (s *Server) imagesDir() string { return filepath.Join(filepath.Dir(s.cfg.DBPath), "images") }

func (s *Server) imageFile(id string) string { return filepath.Join(s.imagesDir(), id+".img") }

func (s *Server) readImage(id string) (*api.Image, error) {
	if !imageIDRe.MatchString(id) {
		return nil, os.ErrNotExist
	}
	b, err := os.ReadFile(filepath.Join(s.imagesDir(), id+".json"))
	if err != nil {
		return nil, err
	}
	var im api.Image
	if err := json.Unmarshal(b, &im); err != nil {
		return nil, err
	}
	return &im, nil
}

// imageFormat names the compression from the first bytes of the file.
func imageFormat(head []byte) string {
	switch {
	case len(head) >= 2 && head[0] == 0x1f && head[1] == 0x8b:
		return "gz"
	case len(head) >= 6 && string(head[:6]) == "\xfd7zXZ\x00":
		return "xz"
	case len(head) >= 4 && string(head[:4]) == "\x28\xb5\x2f\xfd":
		return "zst"
	}
	return "raw"
}

func (s *Server) handleListImages(w http.ResponseWriter, r *http.Request) {
	out := []api.Image{}
	entries, _ := os.ReadDir(s.imagesDir())
	for _, e := range entries {
		if id, ok := strings.CutSuffix(e.Name(), ".json"); ok {
			if im, err := s.readImage(id); err == nil {
				out = append(out, *im)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	writeJSON(w, http.StatusOK, out)
}

// handleUploadImage stores the request body as a new image. Name, arch and notes come from the query.
func (s *Server) handleUploadImage(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	name := strings.TrimSpace(q.Get("name"))
	if name == "" || len(name) > 80 {
		writeError(w, http.StatusBadRequest, "give the image a name (up to 80 characters)")
		return
	}
	arch := q.Get("arch")
	switch arch {
	case "", "amd64", "arm64", "armv7", "armv6":
	default:
		writeError(w, http.StatusBadRequest, "arch must be amd64, arm64, armv7 or armv6")
		return
	}
	notes := strings.TrimSpace(q.Get("notes"))
	if len(notes) > 500 {
		notes = notes[:500]
	}
	if err := os.MkdirAll(s.imagesDir(), 0o700); err != nil {
		s.internal(w, err)
		return
	}
	var idb [8]byte
	rand.Read(idb[:])
	id := "img_" + hex.EncodeToString(idb[:])
	tmp, err := os.CreateTemp(s.imagesDir(), "upload-")
	if err != nil {
		s.internal(w, err)
		return
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), http.MaxBytesReader(w, r.Body, maxImageBytes))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil || n == 0 {
		writeError(w, http.StatusBadRequest, "the upload failed or was empty")
		return
	}
	f, err := os.Open(tmp.Name())
	if err != nil {
		s.internal(w, err)
		return
	}
	head := make([]byte, 6)
	hn, _ := io.ReadFull(f, head)
	f.Close()
	who := "an admin"
	if i := identityFrom(r); i != nil {
		who = i.Name
	}
	im := api.Image{ID: id, Name: name, Arch: arch, Notes: notes, Format: imageFormat(head[:hn]), Size: n,
		SHA256: hex.EncodeToString(h.Sum(nil)), CreatedBy: who, CreatedAt: time.Now().UTC()}
	meta, _ := json.Marshal(im)
	if err := os.Rename(tmp.Name(), s.imageFile(id)); err != nil {
		s.internal(w, err)
		return
	}
	if err := os.WriteFile(filepath.Join(s.imagesDir(), id+".json"), meta, 0o600); err != nil {
		os.Remove(s.imageFile(id))
		s.internal(w, err)
		return
	}
	s.event(r.Context(), "", "info", "image.upload", fmt.Sprintf("%s: added image %q (%s, %s)", who, name, im.Format, humanSize(n)))
	writeJSON(w, http.StatusCreated, im)
}

func (s *Server) handleDeleteImage(w http.ResponseWriter, r *http.Request) {
	im, err := s.readImage(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, "no such image")
		return
	}
	os.Remove(s.imageFile(im.ID))
	os.Remove(filepath.Join(s.imagesDir(), im.ID+".json"))
	who := "an admin"
	if i := identityFrom(r); i != nil {
		who = i.Name
	}
	s.event(r.Context(), "", "warning", "image.delete", fmt.Sprintf("%s: deleted image %q", who, im.Name))
	w.WriteHeader(http.StatusNoContent)
}

// handleAgentImage lets a device download an image. It is limited to images that exist; the agent
// only asks for one after an admin queued a flash for it.
func (s *Server) handleAgentImage(w http.ResponseWriter, r *http.Request) {
	im, err := s.readImage(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, "no such image")
		return
	}
	f, err := os.Open(s.imageFile(im.ID))
	if err != nil {
		writeError(w, http.StatusNotFound, "image file is missing")
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeContent(w, r, "", im.CreatedAt, f)
}

// handleFlashImage queues image.flash. Typed confirmation is the exact device path.
func (s *Server) handleFlashImage(w http.ResponseWriter, r *http.Request) {
	d, ok := s.lookupDevice(w, r)
	if !ok {
		return
	}
	var req struct {
		Image   string `json:"image"`
		Device  string `json:"device"`
		Confirm string `json:"confirm"`
		Enrol   bool   `json:"enrol"`
		Name    string `json:"name"`
		Server  string `json:"server"`
	}
	if !decode(w, r, &req) {
		return
	}
	if !hasCap(d, api.CapImages) {
		writeError(w, http.StatusConflict, "this device's agent is too old for image rebuilds; update it from the Updates page first")
		return
	}
	if d.Offline {
		writeError(w, http.StatusConflict, d.Name+" is offline")
		return
	}
	im, err := s.readImage(req.Image)
	if err != nil {
		writeError(w, http.StatusBadRequest, "choose an image")
		return
	}
	if !strings.HasPrefix(req.Device, "/dev/") {
		writeError(w, http.StatusBadRequest, "choose a disk")
		return
	}
	if req.Confirm != req.Device {
		writeError(w, http.StatusBadRequest, "type the device path ("+req.Device+") to confirm")
		return
	}
	if im.Arch != "" && d.Host.Arch != "" && im.Arch != d.Host.Arch {
		// The image is for another machine, so a mismatch is normal and only noted: the disk is the target, not this device.
		s.log.Info("image arch differs from the flashing device", "image", im.Arch, "device", d.Host.Arch)
	}
	args := map[string]string{
		"image_id": im.ID, "format": im.Format, "sha256": im.SHA256, "device": req.Device, "confirm": req.Confirm,
	}
	who := "an admin"
	if i := identityFrom(r); i != nil {
		who = i.Name
	}
	args["by"] = who
	args["image_name"] = im.Name
	if req.Enrol {
		srv := strings.TrimRight(strings.TrimSpace(req.Server), "/")
		if srv == "" {
			scheme := "http"
			if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
				scheme = "https"
			}
			if !installHostRE.MatchString(r.Host) {
				writeError(w, http.StatusBadRequest, "invalid Host header")
				return
			}
			srv = scheme + "://" + r.Host
		}
		if !regexp.MustCompile(`^https?://[A-Za-z0-9.\-\[\]:]+$`).MatchString(srv) {
			writeError(w, http.StatusBadRequest, "the server address must be http(s)://host[:port]")
			return
		}
		args["enrol"], args["server"] = "1", srv
		if n := strings.TrimSpace(req.Name); n != "" {
			if !regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`).MatchString(n) {
				writeError(w, http.StatusBadRequest, "the device name may use letters, digits, dot, dash and underscore")
				return
			}
			args["name"] = n
		}
	}
	cmd, err := s.store.CreateCommand(r.Context(), d.ID, api.ActionImageFlash, req.Device, args)
	if err != nil {
		s.internal(w, err)
		return
	}
	if req.Enrol {
		// A token of its own: one use, 48 hours, revocable from Settings. Delivered once in the dispatch.
		enrolMu.Lock()
		_, _, err := s.mintEnrol(r, "Rebuild of "+req.Device+" on "+d.Name, 48*time.Hour, 1, who, cmd.ID, true)
		enrolMu.Unlock()
		if err != nil {
			s.store.CancelCommand(r.Context(), cmd.ID)
			s.internal(w, err)
			return
		}
	}
	s.event(r.Context(), d.ID, "warning", api.ActionImageFlash+".requested", imageSummary(who, args, d.Name, "requested"))
	writeJSON(w, http.StatusAccepted, scrubCommand(cmd))
}

func imageSummary(who string, a map[string]string, device, state string) string {
	enrol := ""
	if a["enrol"] == "1" {
		enrol = " with first-boot registration"
	}
	return fmt.Sprintf("%s: write image %q onto %s%s %s on %s", who, a["image_name"], a["device"], enrol, state, device)
}

// handleGetBoot asks the device for its Raspberry Pi boot configuration.
func (s *Server) handleGetBoot(w http.ResponseWriter, r *http.Request) {
	d, ok := s.lookupDevice(w, r)
	if !ok {
		return
	}
	if !hasCap(d, api.CapImages) || d.Offline {
		writeError(w, http.StatusConflict, "this device is offline or its agent is too old; update it from the Updates page first")
		return
	}
	cmd, err := s.store.CreateCommand(r.Context(), d.ID, api.ActionBootGet, "", nil)
	if err != nil {
		s.internal(w, err)
		return
	}
	done, err := s.waitCommand(r.Context(), cmd.ID, 30*time.Second)
	switch {
	case err != nil:
		writeError(w, http.StatusGatewayTimeout, err.Error())
		return
	case done.Status == api.CommandFailed:
		writeError(w, http.StatusBadGateway, done.Error)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(done.Output))
}

// handleSetBoot stages a new boot order. The device name must be typed back.
func (s *Server) handleSetBoot(w http.ResponseWriter, r *http.Request) {
	d, ok := s.lookupDevice(w, r)
	if !ok {
		return
	}
	var req struct {
		Order   string `json:"order"`
		Confirm string `json:"confirm"`
	}
	if !decode(w, r, &req) {
		return
	}
	order := strings.ToLower(strings.TrimSpace(req.Order))
	if !regexp.MustCompile(`^0x[12467ef]{1,8}$`).MatchString(order) {
		writeError(w, http.StatusBadRequest, "enter a boot order such as 0xf41 (1 SD, 2 network, 4 USB, 6 NVMe, e eMMC, f restart; read right to left)")
		return
	}
	if strings.TrimSpace(req.Confirm) != d.Name {
		writeError(w, http.StatusBadRequest, "type the device name ("+d.Name+") to confirm")
		return
	}
	if !hasCap(d, api.CapImages) || d.Offline {
		writeError(w, http.StatusConflict, "this device is offline or its agent is too old")
		return
	}
	who := "an admin"
	if i := identityFrom(r); i != nil {
		who = i.Name
	}
	cmd, err := s.store.CreateCommand(r.Context(), d.ID, api.ActionBootSet, order, map[string]string{"order": order, "confirm": order, "by": who})
	if err != nil {
		s.internal(w, err)
		return
	}
	s.event(r.Context(), d.ID, "warning", api.ActionBootSet+".requested", fmt.Sprintf("%s: set boot order to %s requested on %s", who, order, d.Name))
	writeJSON(w, http.StatusAccepted, cmd)
}
