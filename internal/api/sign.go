package api

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"strconv"
)

// Signed work. The control plane signs everything that makes an agent do something as root
// (commands, stream requests, agent updates) with its Ed25519 identity key, so an agent no
// longer depends on the network path being trustworthy. The agent pins the key it was given at
// registration. An old server sends no signatures; an agent accepts that until it has seen the
// first signed message from its server, and refuses unsigned work from then on, so a network
// attacker can't strip the signatures.

// UnmarshalJSON keeps the bytes a command arrived as, which is what the signature covers.
func (c *Command) UnmarshalJSON(b []byte) error {
	type plain Command
	var p plain
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	*c = Command(p)
	c.raw = append(c.raw[:0], b...)
	return nil
}

// SigningBytes is the canonical form of a command without its signature: the top-level fields
// sorted by name, each value exactly as sent. Fields an older agent does not know are included,
// so a newer server can add fields without breaking verification.
func (c Command) SigningBytes() ([]byte, error) {
	raw := c.raw
	if len(raw) == 0 {
		cp := c
		cp.Sig = ""
		var err error
		if raw, err = json.Marshal(cp); err != nil {
			return nil, err
		}
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	delete(m, "sig")
	return json.Marshal(m)
}

// SignCommand fills in c.Sig.
func SignCommand(priv ed25519.PrivateKey, c *Command) error {
	c.Sig = ""
	b, err := c.SigningBytes()
	if err != nil {
		return err
	}
	c.Sig = base64.StdEncoding.EncodeToString(ed25519.Sign(priv, append([]byte("orch-command\n"), b...)))
	return nil
}

// VerifyCommand checks c.Sig against the server's public key (base64).
func VerifyCommand(pubB64 string, c Command) bool {
	b, err := c.SigningBytes()
	return err == nil && verify(pubB64, append([]byte("orch-command\n"), b...), c.Sig)
}

// StreamSigningText is what a stream request's signature covers, bound to the device.
func StreamSigningText(deviceID string, r StreamRequest) []byte {
	return []byte("orch-stream\n" + deviceID + "\n" + r.ID + "\n" + r.Kind + "\n" + r.Container + "\n" +
		strconv.Itoa(r.Tail) + "\n" + strconv.Itoa(r.Cols) + "\n" + strconv.Itoa(r.Rows))
}

// UpdateSigningText is what an agent update's signature covers: the build the agent will run.
func UpdateSigningText(arch string, i AgentUpdateInfo) []byte {
	return []byte("orch-update\n" + arch + "\n" + i.Version + "\n" + i.SHA256 + "\n" + i.URL)
}

// SignText signs msg with the server key.
func SignText(priv ed25519.PrivateKey, msg []byte) string {
	return base64.StdEncoding.EncodeToString(ed25519.Sign(priv, msg))
}

// VerifyText checks a signature made by SignText.
func VerifyText(pubB64 string, msg []byte, sig string) bool { return verify(pubB64, msg, sig) }

func verify(pubB64 string, msg []byte, sig string) bool {
	pub, err := base64.StdEncoding.DecodeString(pubB64)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return false
	}
	s, err := base64.StdEncoding.DecodeString(sig)
	return err == nil && ed25519.Verify(pub, msg, s)
}
