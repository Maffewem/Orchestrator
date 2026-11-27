package server

import (
	"net/http"
	"strings"
	"testing"

	"github.com/Maffewem/Orchestrator/internal/api"
	"github.com/Maffewem/Orchestrator/internal/store"
)

func TestDiskActionsAreAdminOnlyConfirmedAndAudited(t *testing.T) {
	h := newHarness(t)
	var settings struct {
		JoinToken string `json:"join_token"`
	}
	h.do("GET", "/api/settings", adminToken, nil, &settings)
	var reg api.RegisterResponse
	h.do("POST", "/api/agent/register", "", api.RegisterRequest{JoinToken: settings.JoinToken, Name: "pi",
		Host: api.HostInfo{Hostname: "pi", Arch: "arm64", Capabilities: []string{api.CapDisks}}}, &reg)
	base := "/api/devices/" + reg.DeviceID

	// Only admins can even look.
	for _, role := range []string{"viewer", "operator"} {
		tok := h.makeUser("u-"+role, role)
		if code := h.do("GET", base+"/disks", tok, nil, nil); code != http.StatusForbidden {
			t.Errorf("%s list: %d", role, code)
		}
		if code := h.do("POST", base+"/disks/format", tok, map[string]string{"device": "/dev/sdb", "fstype": "ext4", "confirm": "/dev/sdb"}, nil); code != http.StatusForbidden {
			t.Errorf("%s format: %d", role, code)
		}
	}
	// The generic commands API can't be used to reach disk actions.
	if code := h.do("POST", base+"/commands", adminToken, map[string]any{"action": api.ActionDiskFormat, "target": "/dev/sdb"}, nil); code != http.StatusBadRequest {
		t.Errorf("generic command: %d", code)
	}
	// Destructive actions need the device path typed back exactly.
	for _, confirm := range []string{"", "yes", "/dev/sdc", "sdb"} {
		if code := h.do("POST", base+"/disks/format", adminToken, map[string]string{"device": "/dev/sdb", "fstype": "ext4", "confirm": confirm}, nil); code != http.StatusBadRequest {
			t.Errorf("confirm %q accepted: %d", confirm, code)
		}
	}
	if code := h.do("POST", base+"/disks/partition", adminToken, map[string]string{"device": "/dev/sdb", "confirm": "nope"}, nil); code != http.StatusBadRequest {
		t.Errorf("partition without confirm: %d", code)
	}
	var cmd api.Command
	if code := h.do("POST", base+"/disks/format", adminToken, map[string]string{"device": "/dev/sdb", "fstype": "ext4", "confirm": "/dev/sdb"}, &cmd); code != http.StatusAccepted {
		t.Fatalf("format: %d", code)
	}
	if cmd.Action != api.ActionDiskFormat || cmd.Args["by"] != "admin token" {
		t.Errorf("unexpected command %+v", cmd)
	}
	// An SMB password reaches the agent but never the API responses or the history afterwards.
	var smb api.Command
	h.do("POST", base+"/disks/share", adminToken, map[string]string{"kind": "smb", "source": "//nas/media", "mountpoint": "/mnt/nas", "username": "me", "password": "hunter2"}, &smb)
	if smb.Args["password"] != "" {
		t.Error("the response must not echo the password")
	}
	var hb api.HeartbeatResponse
	h.do("POST", "/api/agent/heartbeat", reg.AgentToken, api.HeartbeatRequest{}, &hb)
	var sent bool
	for _, c := range hb.Commands {
		if c.ID == smb.ID {
			sent = c.Args["password"] == "hunter2"
		}
	}
	if !sent {
		t.Error("the agent must receive the password")
	}
	h.do("POST", "/api/agent/commands/"+smb.ID+"/result", reg.AgentToken, api.CommandResult{OK: true, Output: "Mounted"}, nil)
	var after api.Command
	h.do("GET", "/api/commands/"+smb.ID, adminToken, nil, &after)
	if after.Args["password"] != "" {
		t.Error("the stored command must not keep the password")
	}
	// Request and outcome are both in the audit trail, with who asked.
	h.do("POST", "/api/agent/commands/"+cmd.ID+"/result", reg.AgentToken, api.CommandResult{Error: "boom"}, nil)
	var events []store.Event
	h.do("GET", "/api/events?device="+reg.DeviceID, adminToken, nil, &events)
	var requested, failed bool
	for _, e := range events {
		requested = requested || (e.Kind == "disk.format.requested" && strings.Contains(e.Message, "admin token: format /dev/sdb as ext4"))
		failed = failed || (e.Kind == "disk.format" && e.Level == "error" && strings.Contains(e.Message, "boom"))
	}
	if !requested || !failed {
		t.Errorf("audit trail incomplete: %+v", events)
	}
}
