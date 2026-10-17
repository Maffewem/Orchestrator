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
}

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
}

// BuildSpec asks the agent to build the image on the device from a git context
// (the Docker engine clones it, so the device needs no git install).
type BuildSpec struct {
	// Remote is a Docker remote build context: https://host/owner/repo.git#<ref>:<dir>.
	// It may embed credentials, so agents must never log it.
	Remote     string `json:"remote"`
	Dockerfile string `json:"dockerfile,omitempty"`
}

// StepsFor returns the stages a deployment of spec goes through, in order. Agent and
// control plane both derive step indexes from this list.
func StepsFor(spec DeploySpec) []string {
	source := "Pulling image"
	if spec.Build != nil {
		source = "Building"
	}
	return []string{"Initializing", source, "Deploying", "Verifying"}
}

// Step indexes into StepsFor.
const (
	StepInit   = 0
	StepSource = 1
	StepDeploy = 2
	StepVerify = 3
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
