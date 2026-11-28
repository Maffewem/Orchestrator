package server

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Maffewem/Orchestrator/internal/api"
)

// diskActions maps the URL verb to the agent action. All of them are admin-only (POST routes
// default to admin) and none is reachable through the generic commands API.
var diskActions = map[string]string{
	"format":    api.ActionDiskFormat,
	"partition": api.ActionDiskPartition,
	"mount":     api.ActionDiskMount,
	"unmount":   api.ActionDiskUnmount,
	"share":     api.ActionShareMount,
}

// handleListDisks asks the device for its live disk list. Nothing is cached, so what an admin
// sees (and then confirms against) is the device's current state.
func (s *Server) handleListDisks(w http.ResponseWriter, r *http.Request) {
	d, ok := s.lookupDevice(w, r)
	if !ok {
		return
	}
	if !hasCap(d, api.CapDisks) {
		writeError(w, http.StatusConflict, "this device's agent is too old for disk management; update it from the Updates page first")
		return
	}
	if d.Offline {
		writeError(w, http.StatusConflict, d.Name+" is offline")
		return
	}
	cmd, err := s.store.CreateCommand(r.Context(), d.ID, api.ActionDiskList, "", nil)
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

func (s *Server) handleDiskAction(w http.ResponseWriter, r *http.Request) {
	d, ok := s.lookupDevice(w, r)
	if !ok {
		return
	}
	verb := r.PathValue("action")
	action, known := diskActions[verb]
	if !known {
		writeError(w, http.StatusNotFound, "unknown disk action")
		return
	}
	var req map[string]string
	if !decode(w, r, &req) {
		return
	}
	if !hasCap(d, api.CapDisks) {
		writeError(w, http.StatusConflict, "this device's agent is too old for disk management; update it from the Updates page first")
		return
	}
	if d.Offline {
		writeError(w, http.StatusConflict, d.Name+" is offline")
		return
	}
	args := map[string]string{}
	for _, k := range []string{"device", "fstype", "label", "layout", "mountpoint", "options", "persist", "confirm", "kind", "source", "username", "password"} {
		if v := strings.TrimSpace(req[k]); v != "" || k == "password" {
			args[k] = v
		}
	}
	if args["password"] == "" {
		delete(args, "password")
	}
	switch action {
	case api.ActionDiskFormat, api.ActionDiskPartition:
		if args["device"] == "" || !strings.HasPrefix(args["device"], "/dev/") {
			writeError(w, http.StatusBadRequest, "choose a disk or partition")
			return
		}
		// Typed confirmation: the admin has to type the exact device path they are about to wipe.
		if args["confirm"] != args["device"] {
			writeError(w, http.StatusBadRequest, "type the device path ("+args["device"]+") to confirm")
			return
		}
	case api.ActionDiskMount:
		if args["device"] == "" || args["mountpoint"] == "" {
			writeError(w, http.StatusBadRequest, "choose a device and a mountpoint")
			return
		}
	case api.ActionDiskUnmount:
		if args["mountpoint"] == "" {
			writeError(w, http.StatusBadRequest, "choose a mountpoint")
			return
		}
	case api.ActionShareMount:
		if args["source"] == "" || args["mountpoint"] == "" {
			writeError(w, http.StatusBadRequest, "enter the share and a mountpoint")
			return
		}
	}
	who := "an admin"
	if id := identityFrom(r); id != nil {
		who = id.Name
	}
	args["by"] = who // audit trail; the agent ignores it
	cmd, err := s.store.CreateCommand(r.Context(), d.ID, action, args["device"]+args["mountpoint"], args)
	if err != nil {
		s.internal(w, err)
		return
	}
	s.event(r.Context(), d.ID, "warning", action+".requested", diskSummary(who, action, args, d.Name, "requested"))
	// The password is only needed by the agent: it is dropped from the stored command once the agent reports back.
	writeJSON(w, http.StatusAccepted, scrubCommand(cmd))
}

func scrubCommand(c *api.Command) *api.Command {
	if c == nil || c.Args["password"] == "" {
		return c
	}
	cp := *c
	cp.Args = map[string]string{}
	for k, v := range c.Args {
		if k != "password" {
			cp.Args[k] = v
		}
	}
	return &cp
}

// diskSummary is the audit line for a disk action.
func diskSummary(who, action string, a map[string]string, device, state string) string {
	what := a["device"]
	switch action {
	case api.ActionDiskFormat:
		what = fmt.Sprintf("format %s as %s", a["device"], a["fstype"])
	case api.ActionDiskPartition:
		what = fmt.Sprintf("re-partition %s (%s)", a["device"], orDefault(a["layout"], "rest"))
	case api.ActionDiskMount:
		what = fmt.Sprintf("mount %s at %s", a["device"], a["mountpoint"])
		if a["persist"] == "1" {
			what += " (at boot)"
		}
	case api.ActionDiskUnmount:
		what = "unmount " + a["mountpoint"]
	case api.ActionShareMount:
		what = fmt.Sprintf("mount %s share %s at %s", a["kind"], a["source"], a["mountpoint"])
	}
	return fmt.Sprintf("%s: %s %s on %s", who, what, state, device)
}
