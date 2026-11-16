package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/Maffewem/Orchestrator/internal/api"
)

func TestReenrollDevice(t *testing.T) {
	ctx := context.Background()
	st, err := Open(filepath.Join(t.TempDir(), "o.db"))
	if err != nil {
		t.Fatal(err)
	}
	host := api.HostInfo{Hostname: "cm5", Addresses: []string{"192.168.0.125"}}
	old, oldTok, _ := st.CreateDevice(ctx, "CM5", nil, host)

	// Still online: never taken over.
	if d, _, _ := st.ReenrollDevice(ctx, "cm5", host); d != nil {
		t.Fatal("online device was taken over")
	}
	st.db.ExecContext(ctx, `UPDATE devices SET offline = 1 WHERE id = ?`, old.ID)

	// Different address: treated as a different device.
	if d, _, _ := st.ReenrollDevice(ctx, "CM5", api.HostInfo{Hostname: "cm5", Addresses: []string{"10.0.0.9"}}); d != nil {
		t.Fatal("matched a different address")
	}
	d, tok, err := st.ReenrollDevice(ctx, "cm5", host)
	if err != nil || d == nil || d.ID != old.ID {
		t.Fatalf("expected the offline device back, got %v %v", d, err)
	}
	if _, err := st.DeviceByToken(ctx, oldTok); err == nil {
		t.Fatal("old token still valid")
	}
	if _, err := st.DeviceByToken(ctx, tok); err != nil {
		t.Fatal("new token not valid")
	}
}
