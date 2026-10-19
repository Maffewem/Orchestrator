// Package api holds the wire types shared by the control plane and the agent.
package api

import "time"

// HostInfo describes a device. Agents send it on registration and with every heartbeat.
type HostInfo struct {
	Hostname     string      `json:"hostname"`
	OS           string      `json:"os"`
	Platform     string      `json:"platform"`
	Arch         string      `json:"arch"`
	Kernel       string      `json:"kernel"`
	Model        string      `json:"model,omitempty"`
	Addresses    []string    `json:"addresses,omitempty"`
	CPUs         int         `json:"cpus"`
	MemoryTotal  uint64      `json:"memory_total"`
	AgentVersion string      `json:"agent_version"`
	Docker       *DockerInfo `json:"docker,omitempty"`
	// InContainer is set when the agent runs as a container, which can't replace its own binary.
	InContainer bool `json:"in_container,omitempty"`
	// Capabilities lists the commands this agent understands. Agents from before a feature
	// shipped don't send it, which is how the control plane knows not to ask them.
	Capabilities []string `json:"capabilities,omitempty"`
	// Server is the control plane URL the agent talks to; the device proxy uses it to ask whether a visitor may enter.
	Server string `json:"server,omitempty"`
}

// Agent capabilities.
const (
	CapBackup   = "backup"    // container.backup and container.restore
	CapUpdate   = "update"    // agent.update
	CapProxy    = "proxy"     // proxy.sync
	CapProxyDNS = "proxy-dns" // proxy.sync with a Cloudflare token (DNS challenge)
	CapPreview  = "preview"   // container.remove
	CapStream   = "stream"    // live log following and container shells
)

// DockerInfo summarises the container engine on a device.
type DockerInfo struct {
	Version           string `json:"version"`
	APIVersion        string `json:"api_version"`
	ContainersRunning int    `json:"containers_running"`
	ContainersTotal   int    `json:"containers_total"`
	Images            int    `json:"images"`
	Error             string `json:"error,omitempty"`
}

// Metrics is a point-in-time resource snapshot of a device.
type Metrics struct {
	CPUPercent    float64 `json:"cpu_percent"`
	MemoryUsed    uint64  `json:"memory_used"`
	MemoryTotal   uint64  `json:"memory_total"`
	DiskUsed      uint64  `json:"disk_used"`
	DiskTotal     uint64  `json:"disk_total"`
	Load1         float64 `json:"load1"`
	Load5         float64 `json:"load5"`
	Load15        float64 `json:"load15"`
	UptimeSeconds uint64  `json:"uptime_seconds"`
}

// Container is the agent's view of one container on its local engine.
type Container struct {
	ID             string            `json:"id"`
	Name           string            `json:"name"`
	Image          string            `json:"image"`
	State          string            `json:"state"`
	Status         string            `json:"status"`
	Created        int64             `json:"created"`
	Ports          []Port            `json:"ports,omitempty"`
	Labels         map[string]string `json:"labels,omitempty"`
	ComposeProject string            `json:"compose_project,omitempty"`
}

// Port is a published or exposed container port.
type Port struct {
	IP          string `json:"ip,omitempty"`
	PrivatePort int    `json:"private_port"`
	PublicPort  int    `json:"public_port,omitempty"`
	Type        string `json:"type"`
}

type RegisterRequest struct {
	JoinToken string   `json:"join_token"`
	Name      string   `json:"name,omitempty"`
	Labels    []string `json:"labels,omitempty"`
	Host      HostInfo `json:"host"`
}

type RegisterResponse struct {
	DeviceID   string `json:"device_id"`
	AgentToken string `json:"agent_token"`
}

type HeartbeatRequest struct {
	Host       HostInfo    `json:"host"`
	Metrics    Metrics     `json:"metrics"`
	Containers []Container `json:"containers"`
}

type HeartbeatResponse struct {
	Commands        []Command `json:"commands"`
	IntervalSeconds int       `json:"interval_seconds"`
}

// Command actions understood by the agent.
const (
	ActionStart   = "container.start"
	ActionStop    = "container.stop"
	ActionRestart = "container.restart"
	ActionLogs    = "container.logs"
	ActionDeploy  = "deploy"
	// ActionBackup streams a container's config and data to the control plane; Args["backup_id"].
	ActionBackup = "container.backup"
	// ActionRestore writes a backup's data back into the existing container; Args["backup_id"].
	ActionRestore = "container.restore"
	// ActionAgentUpdate makes the agent replace its own binary with the control plane's build.
	ActionAgentUpdate = "agent.update"
	// ActionProxySync writes Args["caddyfile"] for the per-device Caddy proxy and (re)starts it.
	ActionProxySync = "proxy.sync"
	// ActionRemove force-removes the container named Target (a preview that is no longer wanted).
	ActionRemove = "container.remove"
)

// AgentUpdateInfo tells an agent which build the control plane serves for its architecture.
type AgentUpdateInfo struct {
	Version string `json:"version"`
	SHA256  string `json:"sha256"`
	URL     string `json:"url"` // path on the control plane
}

// Command is work queued by the control plane for a device's agent.
type Command struct {
	ID        string            `json:"id"`
	DeviceID  string            `json:"device_id"`
	Action    string            `json:"action"`
	Target    string            `json:"target"`
	Args      map[string]string `json:"args,omitempty"`
	Deploy    *DeploySpec       `json:"deploy,omitempty"`
	Status    string            `json:"status"`
	Output    string            `json:"output,omitempty"`
	Error     string            `json:"error,omitempty"`
	CreatedAt time.Time         `json:"created_at"`
	UpdatedAt time.Time         `json:"updated_at"`
}

// Command statuses.
const (
	CommandPending    = "pending"
	CommandDispatched = "dispatched"
	CommandSucceeded  = "succeeded"
	CommandFailed     = "failed"
)

// CommandResult is what the agent reports after running a command.
type CommandResult struct {
	OK     bool   `json:"ok"`
	Output string `json:"output,omitempty"`
	Error  string `json:"error,omitempty"`
}

// DeploySpec describes a service to run as a container on a device.
type DeploySpec struct {
	// Service is the stable name of the deployed thing; it is also the container name.
	Service string `json:"service"`
	Image   string `json:"image"`
	// Ports are Docker-style mappings, e.g. "8080:80" or "53:53/udp".
	Ports   []string `json:"ports,omitempty"`
	Env     []string `json:"env,omitempty"`
	Command []string `json:"command,omitempty"`
	// Restart is a Docker restart policy name: no, always, unless-stopped, on-failure.
	Restart string `json:"restart,omitempty"`
	// Build, when set, builds Image on the device instead of pulling it.
	Build *BuildSpec `json:"build,omitempty"`
	// Labels are added to the container.
	Labels map[string]string `json:"labels,omitempty"`
	// Reuse lets the agent skip the build when Image already exists on the device (rollbacks).
	Reuse bool `json:"reuse,omitempty"`
}

// BuildSpec asks the agent to build the image on the device from a git context
// (the Docker engine clones it, so the device needs no git install).
type BuildSpec struct {
	// Remote is a Docker remote build context: https://host/owner/repo.git#<ref>:<dir>.
	// It may embed credentials, so agents must never log it.
	Remote     string `json:"remote"`
	Dockerfile string `json:"dockerfile,omitempty"`
	// Node, when set, makes the agent clone the repository itself and build a Node.js
	// project with a Dockerfile it generates, instead of using the repo's own Dockerfile.
	Node *NodeSpec `json:"node,omitempty"`
}

// NodeSpec describes how to build a Node.js project. Every field is optional: an empty one
// is detected from the repository (lock file, package.json scripts, .nvmrc, engines).
type NodeSpec struct {
	Install string `json:"install,omitempty"` // e.g. npm ci
	Build   string `json:"build,omitempty"`   // e.g. npm run build
	Start   string `json:"start,omitempty"`   // e.g. npm start
	Version string `json:"version,omitempty"` // Node major or version, e.g. 22, or lts
	// Static makes the project a static site: it is built, then the Output directory
	// (detected from dist, build, out, public when empty) is served by a small web server image on port 80.
	Static bool   `json:"static,omitempty"`
	Output string `json:"output,omitempty"`
}

// StepsFor returns the stages a deployment of spec goes through, in order. Agent and
// control plane both derive step indexes from this list.
func StepsFor(spec DeploySpec) []string {
	switch {
	case IsNode(spec):
		return []string{StepInit, StepCloning, StepInstalling, StepBuilding, StepDeploy, StepVerify}
	case spec.Build != nil:
		return []string{StepInit, StepBuilding, StepDeploy, StepVerify}
	}
	return []string{StepInit, StepPulling, StepDeploy, StepVerify}
}

// IsNode reports whether spec is a Node.js project the agent builds itself.
func IsNode(spec DeploySpec) bool { return spec.Build != nil && spec.Build.Node != nil }

// StepIndex finds a stage of spec's deployment by name, or -1.
func StepIndex(spec DeploySpec, name string) int {
	for i, n := range StepsFor(spec) {
		if n == name {
			return i
		}
	}
	return -1
}

// Deployment stage names, as shown on the deployment page.
const (
	StepInit       = "Initializing"
	StepPulling    = "Pulling image"
	StepCloning    = "Cloning"
	StepInstalling = "Installing"
	StepBuilding   = "Building"
	StepDeploy     = "Deploying"
	StepVerify     = "Verifying"
)

// Step statuses (deployments reuse the queued/running/succeeded/failed vocabulary).
const (
	StepPending   = "pending"
	StepRunning   = "running"
	StepSucceeded = "succeeded"
	StepFailed    = "failed"
	StepSkipped   = "skipped"
)

// StepUpdate is sent by the agent while a deployment step progresses.
type StepUpdate struct {
	Status string `json:"status,omitempty"`
	// Lines are appended to the step's log.
	Lines []LogLine `json:"lines,omitempty"`
}

// LogLine is one timestamped line of deployment output.
type LogLine struct {
	Time time.Time `json:"time"`
	Text string    `json:"text"`
}

// Stream kinds. A stream is a live, two-way pipe between the dashboard and a container,
// carried over plain HTTP long-polls so it works through any proxy.
const (
	StreamLogs  = "logs"
	StreamShell = "shell"
)

// StreamRequest is a stream the control plane wants the agent to open.
type StreamRequest struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	Container string `json:"container"`
	Tail      int    `json:"tail,omitempty"`
	Cols      int    `json:"cols,omitempty"`
	Rows      int    `json:"rows,omitempty"`
}

// StreamPoll is the answer to the agent's long-poll for new streams.
type StreamPoll struct {
	Streams []StreamRequest `json:"streams"`
}

// StreamOutput carries bytes from the container to the dashboard. Closed ends the stream;
// Error says why it ended badly.
type StreamOutput struct {
	Data   []byte `json:"data,omitempty"`
	Closed bool   `json:"closed,omitempty"`
	Error  string `json:"error,omitempty"`
}

// StreamInput carries keystrokes and window size from the dashboard to the container.
type StreamInput struct {
	Data   []byte `json:"data,omitempty"`
	Cols   int    `json:"cols,omitempty"`
	Rows   int    `json:"rows,omitempty"`
	Closed bool   `json:"closed,omitempty"`
}
