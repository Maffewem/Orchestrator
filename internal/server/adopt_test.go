package server

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/Maffewem/Orchestrator/internal/api"
)

// answerInspects plays the agent: it heartbeats until it has been asked to inspect n containers
// and answers each from specs (keyed by container ID).
func answerInspects(h *harness, token string, hb api.HeartbeatRequest, specs map[string]api.ContainerSpec, n int) {
	for done := 0; done < n; {
		var res api.HeartbeatResponse
		h.do("POST", "/api/agent/heartbeat", token, hb, &res)
		for _, c := range res.Commands {
			if c.Action != api.ActionInspect {
				continue
			}
			b, _ := json.Marshal(specs[c.Target])
			h.do("POST", "/api/agent/commands/"+c.ID+"/result", token, api.CommandResult{OK: true, Output: string(b)}, nil)
			done++
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestAdoptContainers(t *testing.T) {
	h := newHarness(t)
	reg := h.register("vm-1")
	hb := api.HeartbeatRequest{
		Host: api.HostInfo{Addresses: []string{"10.0.0.5"}, Capabilities: []string{api.CapInspect}},
		Containers: []api.Container{
			{ID: "c1", Name: "Portainer_UI", Image: "portainer/portainer-ce:2.21", State: "running"},
			{ID: "c2", Name: "db", Image: "postgres:16", State: "running", ComposeProject: "shop"},
			{ID: "c3", Name: "orchestrator-agent", Image: "x", State: "running"},
			{ID: "c4", Name: "managed", Image: "y", State: "running", Labels: map[string]string{"orchestrator.project": "prj_x"}},
		},
	}
	h.do("POST", "/api/agent/heartbeat", reg.AgentToken, hb, nil)
	specs := map[string]api.ContainerSpec{
		"c1": {Name: "Portainer_UI", Image: "portainer/portainer-ce:2.21", Env: []string{"A=1"}, Ports: []string{"9443:9443"},
			Volumes: []string{"portainer_data:/data", "/var/run/docker.sock:/var/run/docker.sock"}, Restart: "always"},
		"c2": {Name: "db", Image: "postgres:16", Restart: "unless-stopped", Unsupported: []string{"memory limit"}},
	}

	adopt := func(dry bool) (int, []AdoptItem) {
		var items []AdoptItem
		done := make(chan int)
		go func() {
			done <- h.do("POST", "/api/devices/"+reg.DeviceID+"/adopt", adminToken, map[string]any{
				"dry_run":    dry,
				"containers": []map[string]string{{"id": "c1"}, {"id": "c2"}, {"id": "c3"}, {"id": "c4"}, {"id": "nope"}},
			}, &items)
		}()
		answerInspects(h, reg.AgentToken, hb, specs, 2)
		return <-done, items
	}

	// Without the capability the agent is too old.
	old := hb
	old.Host.Capabilities = nil
	h.do("POST", "/api/agent/heartbeat", reg.AgentToken, old, nil)
	if code := h.do("POST", "/api/devices/"+reg.DeviceID+"/adopt", adminToken, map[string]any{"containers": []map[string]string{{"id": "c1"}}}, nil); code != http.StatusConflict {
		t.Fatalf("old agent: %d", code)
	}
	h.do("POST", "/api/agent/heartbeat", reg.AgentToken, hb, nil)

	code, items := adopt(true)
	if code != 200 || len(items) != 5 {
		t.Fatalf("dry run: %d %+v", code, items)
	}
	want := []string{AdoptReady, AdoptReady, AdoptSkipped, AdoptSkipped, AdoptFailed}
	for i, it := range items {
		if it.Status != want[i] {
			t.Fatalf("item %d (%s): %s %s", i, it.ContainerName, it.Status, it.Reason)
		}
	}
	if items[0].ProjectName != "portainer-ui" || len(items[1].Warnings) != 1 {
		t.Fatalf("names/warnings: %+v %+v", items[0], items[1])
	}
	var list []ProjectView
	h.do("GET", "/api/projects", adminToken, nil, &list)
	if len(list) != 0 {
		t.Fatal("dry run created projects")
	}

	if code, items = adopt(false); code != 200 || items[0].Status != AdoptAdopted || items[1].Status != AdoptAdopted {
		t.Fatalf("adopt: %d %+v", code, items)
	}
	var p ProjectView
	if code := h.do("GET", "/api/projects/"+items[0].ProjectID, adminToken, nil, &p); code != 200 {
		t.Fatalf("get: %d", code)
	}
	r := p.Runtime
	if p.Source.Type != "image" || p.Source.Image != "portainer/portainer-ce:2.21" || !r.Adopted || r.Container != "Portainer_UI" ||
		r.Restart != "always" || len(r.Volumes) != 2 || r.Env[0] != "A=1" {
		t.Fatalf("project: %+v %+v", p.Source, r)
	}
	spec, err := specFor(p.Project, nil)
	if err != nil || spec.Service != "Portainer_UI" || spec.Volumes[0] != "portainer_data:/data" || spec.Labels["orchestrator.project"] != p.ID {
		t.Fatalf("spec: %+v %v", spec, err)
	}

	// The container is linked to its project before it is ever redeployed.
	var fleet []fleetContainer
	h.do("GET", "/api/containers", adminToken, nil, &fleet)
	if fleet[0].ProjectID != p.ID {
		t.Fatalf("fleet link: %+v", fleet[0])
	}

	// Adopting again skips; the first deployment needs confirmation because it recreates the container.
	if code, items = adopt2(h, reg.DeviceID); code != 200 || items[0].Status != AdoptSkipped {
		t.Fatalf("again: %d %+v", code, items)
	}
	if code := h.do("POST", "/api/projects/"+p.ID+"/deployments", adminToken, nil, nil); code != http.StatusConflict {
		t.Fatalf("deploy without confirm: %d", code)
	}
	if code := h.do("POST", "/api/projects/"+p.ID+"/deployments?confirm=1", adminToken, nil, nil); code != http.StatusAccepted {
		t.Fatalf("deploy with confirm: %d", code)
	}
}

func adopt2(h *harness, device string) (int, []AdoptItem) {
	var items []AdoptItem
	code := h.do("POST", "/api/devices/"+device+"/adopt", adminToken, map[string]any{"containers": []map[string]string{{"id": "c1"}}}, &items)
	return code, items
}
