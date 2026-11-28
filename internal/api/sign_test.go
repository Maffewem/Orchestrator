package api

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"testing"
)

func TestCommandSignatureSurvivesTheWire(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	pubB64 := base64.StdEncoding.EncodeToString(pub)
	c := Command{ID: "cmd_1", DeviceID: "dev_1", Action: "deploy", Target: "dep_1", Args: map[string]string{"b": "2", "a": "1"},
		Deploy: &DeploySpec{Service: "web", Image: "nginx", Env: []string{"A=b"}}}
	if err := SignCommand(priv, &c); err != nil {
		t.Fatal(err)
	}
	wire, _ := json.Marshal(c)
	var got Command
	if err := json.Unmarshal(wire, &got); err != nil {
		t.Fatal(err)
	}
	if !VerifyCommand(pubB64, got) {
		t.Fatal("a signed command did not verify after the round trip")
	}
	// A newer server adds a field this agent has never heard of: the signature must still hold.
	var m map[string]json.RawMessage
	json.Unmarshal(wire, &m)
	m["future"] = json.RawMessage(`{"x":1}`)
	// The extra field was not signed, so this must fail...
	withExtra, _ := json.Marshal(m)
	var tampered Command
	json.Unmarshal(withExtra, &tampered)
	if VerifyCommand(pubB64, tampered) {
		t.Fatal("an added field went unnoticed")
	}
	// ...while a field the server did sign verifies even though the agent's struct lacks it.
	var base map[string]json.RawMessage
	json.Unmarshal(wire, &base)
	delete(base, "sig")
	base["future"] = json.RawMessage(`{"x":1}`)
	body, _ := json.Marshal(base)
	sig := SignText(priv, append([]byte("orch-command\n"), body...))
	base["sig"], _ = json.Marshal(sig)
	signed, _ := json.Marshal(base)
	var fromNewer Command
	json.Unmarshal(signed, &fromNewer)
	if !VerifyCommand(pubB64, fromNewer) {
		t.Fatal("a signed field unknown to the agent broke verification")
	}
	for name, mutate := range map[string]func(*Command){
		"target": func(c *Command) { c.Target = "dep_2" }, "device": func(c *Command) { c.DeviceID = "dev_2" },
		"env": func(c *Command) { c.Deploy.Env = []string{"A=evil"} }, "args": func(c *Command) { c.Args["a"] = "x" },
	} {
		var x Command
		json.Unmarshal(wire, &x)
		mutate(&x)
		w2, _ := json.Marshal(x)
		var y Command
		json.Unmarshal(w2, &y)
		if VerifyCommand(pubB64, y) {
			t.Errorf("changing %s was not detected", name)
		}
	}
	otherPub, _, _ := ed25519.GenerateKey(rand.Reader)
	if VerifyCommand(base64.StdEncoding.EncodeToString(otherPub), got) {
		t.Fatal("verified against the wrong key")
	}
}
