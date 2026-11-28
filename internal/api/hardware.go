package api

// Device LEDs, the host console and SSH control. See internal/agent/leds_linux.go and sshctl_linux.go.

// LED profiles. A profile says how the agent drives a device's LEDs.
const (
	// LEDProfilePi uses the kernel's LED class (/sys/class/leds): the activity and power LED
	// of a Raspberry Pi, and any other LED the board's device tree exposes.
	LEDProfilePi = "pi"
	// LEDProfileBlade is reserved for Compute Blade style carriers, whose LEDs are not on the
	// kernel's LED class. It is a hook only: see ledProfiles in the agent.
	LEDProfileBlade = "blade"
)

// LED rule metrics.
const (
	LEDMetricTemp   = "temp"   // hottest thermal zone, °C
	LEDMetricMemory = "memory" // memory in use, %
	LEDMetricCPU    = "cpu"    // CPU busy, %
	LEDMetricDisk   = "disk"   // root filesystem in use, %
)

// LED is one LED the agent found.
type LED struct {
	Name       string   `json:"name"`
	Role       string   `json:"role"` // activity, power, status (a blade LED) or other
	Brightness int      `json:"brightness"`
	Max        int      `json:"max"`
	Trigger    string   `json:"trigger,omitempty"` // what normally drives it (for example mmc0, default-on)
	Triggers   []string `json:"triggers,omitempty"`
	// State is what the agent is showing on it now: "" (left alone), "ok", "warn", "crit" or "identify".
	State string `json:"state,omitempty"`
}

// LEDRule turns one metric into a state of one LED: a slow blink from Warn, a fast one from Crit.
type LEDRule struct {
	Metric string  `json:"metric"`
	LED    string  `json:"led"`
	Warn   float64 `json:"warn"`
	Crit   float64 `json:"crit"`
}

// LEDConfig is a device's LED behaviour. The agent keeps a copy and applies it by itself, so
// it keeps working while the server is away.
type LEDConfig struct {
	Profile string `json:"profile"`
	Enabled bool   `json:"enabled"`
	// Stealth (shown as "Lights off" in the dashboard; the stored name is kept so saved settings still load) keeps every LED of the profile off, whatever the rules say (identify still works).
	Stealth bool      `json:"stealth,omitempty"`
	Rules   []LEDRule `json:"rules,omitempty"`
	// Colours sets what colour each state shows and how bright, on LEDs that can do colour (the Compute
	// Blade's). Ordinary Pi LEDs are on or off and ignore it.
	Colours *LEDColours `json:"colours,omitempty"`
}

// LEDColours are "#rrggbb" values; an empty one keeps the default (normal off, warning amber, critical
// red, identify blue).
type LEDColours struct {
	Normal   string `json:"normal,omitempty"`
	Warn     string `json:"warn,omitempty"`
	Crit     string `json:"crit,omitempty"`
	Identify string `json:"identify,omitempty"`
	// Brightness is a percentage from 1 to 100; 0 means full.
	Brightness int `json:"brightness,omitempty"`
}

// LEDStatus is the agent's answer to led.list.
type LEDStatus struct {
	Profile string `json:"profile"`
	// Supported is false when the profile has no way to drive LEDs on this device; Note says why.
	Supported bool   `json:"supported"`
	Note      string `json:"note,omitempty"`
	// Colours is true when the LEDs can show colours and brightness, so the dashboard offers those controls.
	Colours bool      `json:"colours,omitempty"`
	LEDs    []LED     `json:"leds"`
	Config  LEDConfig `json:"config"`
	// Values are the metrics the rules last saw, by metric name.
	Values map[string]float64 `json:"values,omitempty"`
}

// SSH settings an administrator can change from the dashboard.
type SSHSettings struct {
	PasswordAuth bool   `json:"password_auth"`
	PubkeyAuth   bool   `json:"pubkey_auth"`
	RootLogin    string `json:"root_login"` // yes, no, prohibit-password or forced-commands-only
}

// SSHKey is one line of an authorized_keys file.
type SSHKey struct {
	User        string `json:"user"`
	Type        string `json:"type"`
	Fingerprint string `json:"fingerprint"`
	Comment     string `json:"comment,omitempty"`
}

// SSHUser is an account that can sign in over SSH.
type SSHUser struct {
	Name string `json:"name"`
	UID  int    `json:"uid"`
	Home string `json:"home"`
}

// SSHStatus is the agent's answer to ssh.status.
type SSHStatus struct {
	Installed bool        `json:"installed"`
	Running   bool        `json:"running"`
	Service   string      `json:"service,omitempty"`
	Effective SSHSettings `json:"effective"`
	// Managed is true when the settings come from the file Orchestrator manages.
	Managed bool      `json:"managed"`
	Users   []SSHUser `json:"users,omitempty"`
	Keys    []SSHKey  `json:"keys,omitempty"`
	// Pending is set while a change is on trial: it reverts at RevertAt (RFC 3339) unless confirmed.
	Pending *SSHPending `json:"pending,omitempty"`
	Error   string      `json:"error,omitempty"`
}

// SSHPending describes a change that will be undone unless someone confirms it.
type SSHPending struct {
	RevertAt string `json:"revert_at"`
}

// SSHWarning is a reason a change could lock someone out. Lockout warnings need a typed confirmation.
type SSHWarning struct {
	Level   string `json:"level"` // lockout or note
	Message string `json:"message"`
}

// SSHCheck is the agent's answer to ssh.check, and what ssh.config refuses to continue past without Args["confirm"]=="1".
type SSHCheck struct {
	Warnings []SSHWarning `json:"warnings"`
	// Lockout is true when any warning is at level lockout.
	Lockout bool `json:"lockout"`
}

// Power sources a blade can report (Compute Blade: GPIO23 low means PoE+ or PoE++, high means 5 V only).
const (
	PowerPoEPlus = "poe-plus" // PoE+ or PoE++ present, up to 30 W
	PowerBasic   = "5v"       // 5 V present from PoE or USB-C, up to 15 W
)

// PowerSource is what a Compute Blade's GPIO23 says about its supply. It is not a measurement of use.
type PowerSource struct {
	Source   string `json:"source"`
	MaxWatts int    `json:"max_watts"`
}
