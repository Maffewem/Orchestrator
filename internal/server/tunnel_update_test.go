package server

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Maffewem/Orchestrator/internal/api"
)

func TestVersionOlder(t *testing.T) {
	for _, c := range []struct {
		have, latest string
		want         bool
	}{
		{"2025.2.0", "2025.11.1", true},
		{"2025.11.1", "2025.11.1", false},
		{"2025.11.2", "2025.11.1", false},
		{"2024.12.2", "v2025.1.0", true},
		{"2025.11", "2025.11.1", true},
		{"", "2025.11.1", false},
		{"2025.11.1", "", false},
		{"garbage", "2025.11.1", false},
	} {
		if got := versionOlder(c.have, c.latest); got != c.want {
			t.Errorf("versionOlder(%q, %q) = %v, want %v", c.have, c.latest, got, c.want)
		}
	}
}

func TestFetchCloudflaredLatest(t *testing.T) {
	h := newHarness(t)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"tag_name":"2025.11.1"}`))
	}))
	defer api.Close()
	h.srv.cfdURL = api.URL
	if v, err := h.srv.fetchCloudflaredLatest(); err != nil || v != "2025.11.1" {
		t.Fatalf("got %q, %v", v, err)
	}
}

func TestTunnelAutoUpdate(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	reg := h.register("pi-a")
	tok := base64.StdEncoding.EncodeToString([]byte(`{"a":"acct","t":"t1"}`))
	if err := h.srv.saveTunnelConfig(ctx, reg.DeviceID, &tunnelConfig{Token: tok}); err != nil {
		t.Fatal(err)
	}
	report := func(version string, caps ...string) {
		d, _ := h.st.GetDevice(ctx, reg.DeviceID)
		host := d.Host
		host.Capabilities = caps
		host.Tunnel = &api.TunnelInfo{Version: version}
		if err := h.st.UpdateDeviceState(ctx, reg.DeviceID, host, d.Metrics, nil); err != nil {
			t.Fatal(err)
		}
	}
	pending := func() []api.Command {
		cmds, _ := h.st.DispatchCommands(ctx, reg.DeviceID)
		var out []api.Command
		for _, c := range cmds {
			if c.Action == api.ActionTunnelSync {
				out = append(out, c)
			}
		}
		return out
	}
	h.srv.cfdLatest, h.srv.cfdTried = "2025.11.1", time.Now()
	now := time.Now()

	// Not opted in: nothing happens, but the view says an update is available.
	report("2025.2.0", api.CapTunnel, api.CapTunnelUpdate)
	h.srv.tunnelUpdatePass(ctx, now)
	if len(pending()) != 0 {
		t.Fatal("updated without opting in")
	}
	d, _ := h.st.GetDevice(ctx, reg.DeviceID)
	if v := h.srv.tunnelView(ctx, d); !v.UpdateAvailable || v.Version != "2025.2.0" || v.Latest != "2025.11.1" || !v.CanUpdate {
		t.Fatalf("view %+v", v)
	}

	cfg, _ := h.srv.tunnelConfig(ctx, reg.DeviceID)
	cfg.AutoUpdate = true
	h.srv.saveTunnelConfig(ctx, reg.DeviceID, &cfg)

	// Old agent without the capability is skipped.
	report("2025.2.0", api.CapTunnel)
	h.srv.tunnelUpdatePass(ctx, now)
	if len(pending()) != 0 {
		t.Fatal("updated through an agent that cannot")
	}

	report("2025.2.0", api.CapTunnel, api.CapTunnelUpdate)
	h.srv.tunnelUpdatePass(ctx, now)
	cmds := pending()
	if len(cmds) != 1 || cmds[0].Args["update"] != "1" || cmds[0].Args["token"] != tok {
		t.Fatalf("commands %+v", cmds)
	}
	// Tried recently: not repeated.
	h.srv.tunnelUpdatePass(ctx, now.Add(time.Hour))
	if len(pending()) != 0 {
		t.Fatal("repeated too soon")
	}
	// Up to date: nothing even after the retry window.
	report("2025.11.1", api.CapTunnel, api.CapTunnelUpdate)
	h.srv.tunnelUpdatePass(ctx, now.Add(24*time.Hour))
	if len(pending()) != 0 {
		t.Fatal("updated a current tunnel")
	}
	// Maintenance mode holds automatic updates back.
	report("2025.2.0", api.CapTunnel, api.CapTunnelUpdate)
	if err := h.st.SetDeviceMaintenance(ctx, reg.DeviceID, true, "", time.Time{}); err != nil {
		t.Fatal(err)
	}
	h.srv.tunnelUpdatePass(ctx, now.Add(48*time.Hour))
	if len(pending()) != 0 {
		t.Fatal("updated during maintenance")
	}
}

func TestManualTunnelUpdate(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	reg := h.register("pi-a")
	var out tunnelView
	if code := h.do("POST", "/api/devices/"+reg.DeviceID+"/tunnel/update", adminToken, nil, &out); code != http.StatusConflict {
		t.Fatalf("no tunnel: %d", code)
	}
	tok := base64.StdEncoding.EncodeToString([]byte(`{"a":"acct","t":"t1"}`))
	h.srv.saveTunnelConfig(ctx, reg.DeviceID, &tunnelConfig{Token: tok})
	if code := h.do("POST", "/api/devices/"+reg.DeviceID+"/tunnel/update", adminToken, nil, &out); code != http.StatusConflict {
		t.Fatalf("old agent: %d", code)
	}
	d, _ := h.st.GetDevice(ctx, reg.DeviceID)
	host := d.Host
	host.Capabilities = []string{api.CapTunnel, api.CapTunnelUpdate}
	h.st.UpdateDeviceState(ctx, reg.DeviceID, host, d.Metrics, nil)
	if code := h.do("POST", "/api/devices/"+reg.DeviceID+"/tunnel/update", adminToken, nil, &out); code != http.StatusAccepted {
		t.Fatalf("update: %d", code)
	}
	if code := h.do("PUT", "/api/devices/"+reg.DeviceID+"/tunnel/auto-update", adminToken, map[string]bool{"enabled": true}, &out); code != http.StatusOK || !out.AutoUpdate {
		t.Fatalf("auto: %d %+v", code, out)
	}
	cfg, _ := h.srv.tunnelConfig(ctx, reg.DeviceID)
	if !cfg.AutoUpdate || cfg.Token != tok {
		t.Fatalf("config %+v", cfg)
	}
}

func TestTunnelAutoUpdateFollowsUpdateSchedule(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	reg := h.register("pi-a")
	tok := base64.StdEncoding.EncodeToString([]byte(`{"a":"acct","t":"t1"}`))
	h.srv.saveTunnelConfig(ctx, reg.DeviceID, &tunnelConfig{Token: tok, AutoUpdate: true})
	d, _ := h.st.GetDevice(ctx, reg.DeviceID)
	host := d.Host
	host.Capabilities = []string{api.CapTunnel, api.CapTunnelUpdate}
	host.Tunnel = &api.TunnelInfo{Version: "2025.2.0"}
	h.st.UpdateDeviceState(ctx, reg.DeviceID, host, d.Metrics, nil)
	h.srv.cfdLatest, h.srv.cfdTried = "2025.11.1", time.Now()

	count := func() int {
		cmds, _ := h.st.DispatchCommands(ctx, reg.DeviceID)
		n := 0
		for _, c := range cmds {
			if c.Action == api.ActionTunnelSync && c.Args["update"] == "1" {
				n++
			}
		}
		return n
	}
	now := time.Date(2026, 11, 18, 4, 0, 0, 0, time.UTC)
	h.srv.store.SetSetting(ctx, settingUpdateSetAt, now.Add(-48*time.Hour).Format(time.RFC3339))

	// Schedule off: nothing runs, and the view says why.
	h.srv.scheduleTick(ctx, now)
	if count() != 0 || h.srv.tunnelUpdateWhen(ctx) != "" {
		t.Fatal("ran with the schedule off")
	}
	h.srv.setJSON(ctx, settingUpdateSchedule, Schedule{Mode: "daily", Time: "03:00", TZ: "UTC"})
	if got := h.srv.tunnelUpdateWhen(ctx); got != "daily at 03:00 UTC" {
		t.Fatalf("when %q", got)
	}
	// Before the slot nothing happens; once it has passed the tunnel update is queued.
	h.srv.scheduleTick(ctx, time.Date(2026, 11, 16, 2, 0, 0, 0, time.UTC))
	if count() != 0 {
		t.Fatal("ran before the slot")
	}
	h.srv.scheduleTick(ctx, now)
	if count() != 1 {
		t.Fatal("not queued when the schedule fired")
	}
}
