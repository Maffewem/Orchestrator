package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// First-boot enrolment. A rebuilt machine has no agent yet, so a small systemd job in the new
// root filesystem fetches the installer from this server and registers it with the join token.
// The job and its token file delete themselves once the agent has registered.

var (
	enrolServerRe = regexp.MustCompile(`^https?://[A-Za-z0-9.\-\[\]:]+$`)
	enrolTokenRe  = regexp.MustCompile(`^[A-Za-z0-9_.\-]{8,200}$`)
	enrolNameRe   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)
)

const enrolScript = `#!/bin/sh
# Written by Orchestrator when this disk was imaged. Registers the machine, then removes itself.
set -u
. /etc/orchestrator-firstboot/env
i=0
until curl -fsSL "$ORCH_SERVER/install.sh" -o /tmp/orch-install.sh; do
  i=$((i+1)); [ "$i" -gt 60 ] && exit 1; sleep 10
done
ARGS="--token $ORCH_TOKEN"
[ -n "${ORCH_NAME:-}" ] && ARGS="$ARGS --name $ORCH_NAME"
if sh /tmp/orch-install.sh $ARGS; then
  rm -rf /etc/orchestrator-firstboot /etc/systemd/system/orchestrator-firstboot.service \
    /etc/systemd/system/multi-user.target.wants/orchestrator-firstboot.service
fi
rm -f /tmp/orch-install.sh
`

const enrolUnit = `[Unit]
Description=Register this machine with Orchestrator (first boot)
After=network-online.target
Wants=network-online.target
ConditionPathExists=/etc/orchestrator-firstboot/env

[Service]
Type=oneshot
ExecStart=/bin/sh /etc/orchestrator-firstboot/enrol.sh
TimeoutStartSec=infinity

[Install]
WantedBy=multi-user.target
`

// validateEnrol checks the enrolment details before anything is written.
func validateEnrol(server, token, name string) error {
	if !enrolServerRe.MatchString(server) {
		return fmt.Errorf("%q is not a usable server address", server)
	}
	if !enrolTokenRe.MatchString(token) {
		return fmt.Errorf("the join token has an unexpected format")
	}
	if name != "" && !enrolNameRe.MatchString(name) {
		return fmt.Errorf("%q is not a valid device name", name)
	}
	return nil
}

// installFirstBoot writes the enrolment job into the root filesystem mounted at root.
func installFirstBoot(root, server, token, name string) error {
	if err := validateEnrol(server, token, name); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(root, "etc")); err != nil {
		return fmt.Errorf("%s does not look like a Linux root filesystem", root)
	}
	dir := filepath.Join(root, "etc/orchestrator-firstboot")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	env := fmt.Sprintf("ORCH_SERVER=%s\nORCH_TOKEN=%s\nORCH_NAME=%s\n", server, token, name)
	if err := os.WriteFile(filepath.Join(dir, "env"), []byte(env), 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "enrol.sh"), []byte(enrolScript), 0o700); err != nil {
		return err
	}
	unitDir := filepath.Join(root, "etc/systemd/system")
	if err := os.MkdirAll(filepath.Join(unitDir, "multi-user.target.wants"), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(unitDir, "orchestrator-firstboot.service"), []byte(enrolUnit), 0o644); err != nil {
		return err
	}
	link := filepath.Join(unitDir, "multi-user.target.wants/orchestrator-firstboot.service")
	os.Remove(link)
	return os.Symlink("/etc/systemd/system/orchestrator-firstboot.service", link)
}

// bootOrderRe matches the EEPROM setting, e.g. BOOT_ORDER=0xf41.
var bootOrderRe = regexp.MustCompile(`(?m)^\s*BOOT_ORDER\s*=\s*(0x[0-9a-fA-F]{1,8})\s*$`)

// bootOrderValid accepts 0x followed by up to 8 hex digits from the documented set
// (1 SD, 2 network, 4 USB mass storage, 6 NVMe, e eMMC, f restart).
var bootOrderValid = regexp.MustCompile(`^0x[12467ef]{1,8}$`)

func parseBootOrder(cfg string) string {
	if m := bootOrderRe.FindStringSubmatch(cfg); m != nil {
		return strings.ToLower(m[1])
	}
	return ""
}

// withBootOrder returns cfg with BOOT_ORDER set to order, adding the line when absent.
func withBootOrder(cfg, order string) string {
	line := "BOOT_ORDER=" + order
	if bootOrderRe.MatchString(cfg) {
		return bootOrderRe.ReplaceAllString(cfg, line)
	}
	if cfg != "" && !strings.HasSuffix(cfg, "\n") {
		cfg += "\n"
	}
	return cfg + line + "\n"
}
