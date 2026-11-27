package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Maffewem/Orchestrator/internal/api"
	"github.com/Maffewem/Orchestrator/internal/store"
)

func TestGoldImagesAndRebuild(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	srv, _, err := New(context.Background(), st, Config{AdminToken: adminToken, HeartbeatInterval: time.Second, DBPath: filepath.Join(t.TempDir(), "o.db")})
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, srv: srv, ts: newTestServer(t, srv), st: st}
	var settings struct {
		JoinToken string `json:"join_token"`
	}
	h.do("GET", "/api/settings", adminToken, nil, &settings)
	var reg api.RegisterResponse
	h.do("POST", "/api/agent/register", "", api.RegisterRequest{JoinToken: settings.JoinToken, Name: "pi",
		Host: api.HostInfo{Hostname: "pi", Arch: "arm64", Capabilities: []string{api.CapImages}}}, &reg)
	base := "/api/devices/" + reg.DeviceID

	upload := func(token string, data []byte, query string) (int, api.Image) {
		req, _ := http.NewRequest("POST", h.ts.URL+"/api/images?"+query, bytes.NewReader(data))
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var im api.Image
		json.NewDecoder(resp.Body).Decode(&im)
		return resp.StatusCode, im
	}

	// Only admins can store or even list images.
	for _, role := range []string{"viewer", "operator"} {
		tok := h.makeUser("u-"+role, role)
		if code, _ := upload(tok, []byte("x"), "name=a"); code != http.StatusForbidden {
			t.Errorf("%s upload: %d", role, code)
		}
		if code := h.do("GET", "/api/images", tok, nil, nil); code != http.StatusForbidden {
			t.Errorf("%s list: %d", role, code)
		}
		if code := h.do("GET", base+"/boot", tok, nil, nil); code != http.StatusForbidden {
			t.Errorf("%s boot: %d", role, code)
		}
	}
	if code, _ := upload(adminToken, []byte("x"), ""); code != http.StatusBadRequest {
		t.Errorf("nameless upload: %d", code)
	}

	data := append([]byte{0x1f, 0x8b}, bytes.Repeat([]byte("disk"), 1000)...)
	code, im := upload(adminToken, data, "name=Pi+base&arch=arm64&notes=golden")
	if code != http.StatusCreated {
		t.Fatalf("upload: %d", code)
	}
	sum := sha256.Sum256(data)
	if im.Format != "gz" || im.Size != int64(len(data)) || im.SHA256 != hex.EncodeToString(sum[:]) {
		t.Errorf("unexpected image %+v", im)
	}
	var list []api.Image
	h.do("GET", "/api/images", adminToken, nil, &list)
	if len(list) != 1 || list[0].ID != im.ID {
		t.Errorf("list: %+v", list)
	}

	// Agents can download it with their own token, strangers cannot.
	get := func(tok, id string) (int, []byte) {
		req, _ := http.NewRequest("GET", h.ts.URL+"/api/agent/images/"+id, nil)
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, b
	}
	if c, b := get(reg.AgentToken, im.ID); c != 200 || !bytes.Equal(b, data) {
		t.Errorf("agent download: %d", c)
	}
	if c, _ := get("", im.ID); c != http.StatusUnauthorized {
		t.Errorf("anonymous download: %d", c)
	}
	if c, _ := get(reg.AgentToken, "../etc/passwd"); c != http.StatusNotFound {
		t.Errorf("bad id: %d", c)
	}

	// The typed confirmation must be the exact device path.
	flash := map[string]any{"image": im.ID, "device": "/dev/sdb", "confirm": "sdb", "enrol": true, "name": "new-pi"}
	if code := h.do("POST", base+"/images/flash", adminToken, flash, nil); code != http.StatusBadRequest {
		t.Errorf("wrong confirm accepted: %d", code)
	}
	flash["confirm"] = "/dev/sdb"
	var cmd api.Command
	if code := h.do("POST", base+"/images/flash", adminToken, flash, &cmd); code != http.StatusAccepted {
		t.Fatalf("flash: %d", code)
	}
	// The join token reaches the agent but is never kept in what the dashboard can read.
	var stored api.Command
	h.do("GET", "/api/commands/"+cmd.ID, adminToken, nil, &stored)
	if stored.Args["join_token"] != "" || cmd.Args["join_token"] != "" {
		t.Error("join token stored in the command")
	}
	var hb api.HeartbeatResponse
	h.do("POST", "/api/agent/heartbeat", reg.AgentToken, api.HeartbeatRequest{Host: api.HostInfo{Hostname: "pi", Arch: "arm64", Capabilities: []string{api.CapImages}}}, &hb)
	var got *api.Command
	for i := range hb.Commands {
		if hb.Commands[i].ID == cmd.ID {
			got = &hb.Commands[i]
		}
	}
	if got == nil || !strings.HasPrefix(got.Args["join_token"], "enr_") || got.Args["sha256"] != im.SHA256 || got.Args["name"] != "new-pi" {
		t.Fatalf("agent command: %+v", got)
	}
	h.do("POST", "/api/agent/commands/"+cmd.ID+"/result", reg.AgentToken, api.CommandResult{Error: "boom"}, nil)

	var events []store.Event
	h.do("GET", "/api/events?device="+reg.DeviceID, adminToken, nil, &events)
	var requested, failed bool
	for _, e := range events {
		requested = requested || (e.Kind == "image.flash.requested" && strings.Contains(e.Message, `admin token: write image "Pi base" onto /dev/sdb with first-boot registration`))
		failed = failed || (e.Kind == "image.flash" && e.Level == "error" && strings.Contains(e.Message, "boom"))
	}
	if !requested || !failed {
		t.Errorf("audit trail incomplete: %+v", events)
	}

	// Boot order needs the value typed back and a sane value.
	for _, order := range []string{"f41", "0xz", "0x1f4; reboot"} {
		if code := h.do("POST", base+"/boot", adminToken, map[string]string{"order": order, "confirm": order}, nil); code != http.StatusBadRequest {
			t.Errorf("order %q: %d", order, code)
		}
	}
	if code := h.do("POST", base+"/boot", adminToken, map[string]string{"order": "0xf41", "confirm": "0xf14"}, nil); code != http.StatusBadRequest {
		t.Errorf("mismatched confirm: %d", code)
	}
	if code := h.do("POST", base+"/boot", adminToken, map[string]string{"order": "0xf41", "confirm": "0xf41"}, nil); code != http.StatusAccepted {
		t.Errorf("boot set: %d", code)
	}
	// The generic commands API can't reach any of this.
	for _, a := range []string{api.ActionImageFlash, api.ActionBootSet} {
		if code := h.do("POST", base+"/commands", adminToken, map[string]any{"action": a}, nil); code != http.StatusBadRequest {
			t.Errorf("generic %s: %d", a, code)
		}
	}
	if code := h.do("DELETE", "/api/images/"+im.ID, adminToken, nil, nil); code != http.StatusNoContent {
		t.Errorf("delete: %d", code)
	}
	if c, _ := get(reg.AgentToken, im.ID); c != http.StatusNotFound {
		t.Errorf("deleted image still served: %d", c)
	}
}

func TestEnrolmentTokens(t *testing.T) {
	st, _ := store.Open(":memory:")
	t.Cleanup(func() { st.Close() })
	srv, _, err := New(context.Background(), st, Config{AdminToken: adminToken, HeartbeatInterval: time.Second, DBPath: filepath.Join(t.TempDir(), "o.db")})
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, srv: srv, ts: newTestServer(t, srv), st: st}
	viewer := h.makeUser("viewer-user", "viewer")
	if code := h.do("GET", "/api/enrol-tokens", viewer, nil, nil); code != http.StatusForbidden {
		t.Errorf("viewer list: %d", code)
	}
	if code := h.do("POST", "/api/enrol-tokens", adminToken, map[string]any{"label": ""}, nil); code != http.StatusBadRequest {
		t.Errorf("no label: %d", code)
	}
	if code := h.do("POST", "/api/enrol-tokens", adminToken, map[string]any{"label": "x", "ttl_hours": 24 * 60}, nil); code != http.StatusBadRequest {
		t.Errorf("ttl too long: %d", code)
	}
	var v EnrolTokenView
	if code := h.do("POST", "/api/enrol-tokens", adminToken, map[string]any{"label": "card in drawer", "max_uses": 2}, &v); code != http.StatusCreated || v.Token == "" {
		t.Fatalf("create: %d %+v", code, v)
	}
	reg := func(tok, name string) int {
		return h.do("POST", "/api/agent/register", "", api.RegisterRequest{JoinToken: tok, Name: name, Host: api.HostInfo{Hostname: name}}, nil)
	}
	if reg(v.Token, "a") != http.StatusCreated || reg(v.Token, "b") != http.StatusCreated {
		t.Fatal("two uses should work")
	}
	if code := reg(v.Token, "c"); code != http.StatusUnauthorized {
		t.Errorf("third use: %d", code)
	}
	var list []EnrolTokenView
	h.do("GET", "/api/enrol-tokens", adminToken, nil, &list)
	if len(list) != 1 || list[0].Status != "used up" || len(list[0].Devices) != 2 || list[0].Token != "" {
		t.Errorf("list: %+v", list)
	}
	// Revoked and expired tokens stop working.
	var v2 EnrolTokenView
	h.do("POST", "/api/enrol-tokens", adminToken, map[string]any{"label": "second"}, &v2)
	if code := h.do("DELETE", "/api/enrol-tokens/"+v2.ID, adminToken, nil, nil); code != http.StatusNoContent {
		t.Errorf("revoke: %d", code)
	}
	if code := reg(v2.Token, "d"); code != http.StatusUnauthorized {
		t.Errorf("revoked token worked: %d", code)
	}
	var v3 EnrolTokenView
	h.do("POST", "/api/enrol-tokens", adminToken, map[string]any{"label": "short"}, &v3)
	raw, _ := st.GetSetting(context.Background(), settingEnrolTokens)
	var all []enrolToken
	json.Unmarshal([]byte(raw), &all)
	for i := range all {
		if all[i].ID == v3.ID {
			all[i].ExpiresAt = time.Now().Add(-time.Minute)
		}
	}
	b, _ := json.Marshal(all)
	st.SetSetting(context.Background(), settingEnrolTokens, string(b))
	if code := reg(v3.Token, "e"); code != http.StatusUnauthorized {
		t.Errorf("expired token worked: %d", code)
	}
	if strings.Contains(raw, v.Token) {
		t.Error("the secret must not be stored in plain text")
	}
}
