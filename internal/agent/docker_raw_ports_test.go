package agent

import "testing"

func TestAddPortBindingsKeepsExistingAndAddsNew(t *testing.T) {
	host := map[string]any{"PortBindings": map[string]any{"8080/tcp": []any{map[string]any{"HostPort": "8080"}}}}
	body := map[string]any{"ExposedPorts": map[string]any{"8080/tcp": map[string]any{}}}
	if err := addPortBindings(body, host, []string{"8443:8443", "9999:8080"}); err != nil {
		t.Fatal(err)
	}
	pb := host["PortBindings"].(map[string]any)
	if got := pb["8080/tcp"].([]any)[0].(map[string]any)["HostPort"]; got != "8080" {
		t.Fatalf("existing mapping changed: %v", got)
	}
	if got := pb["8443/tcp"].([]any)[0].(map[string]any)["HostPort"]; got != "8443" {
		t.Fatalf("new mapping missing: %v", got)
	}
	if _, ok := body["ExposedPorts"].(map[string]any)["8443/tcp"]; !ok {
		t.Fatal("port not exposed")
	}
	if addPortBindings(body, host, []string{"nonsense"}) == nil {
		t.Fatal("bad mapping accepted")
	}
}
