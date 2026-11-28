// Package api holds the wire types shared by the control plane and the agent.
package api

import (
	"path"
	"strings"
	"time"
)

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
	// ServerVia says where that URL came from: configured (ORCH_SERVER), announced (the list
	// in Settings) or discovered (found on the local network after the others failed).
	ServerVia string `json:"server_via,omitempty"`
	// Network details each address: its interface and, where the agent can tell, whether it
	// was handed out by DHCP and so may change.
	Network []NetAddr `json:"network,omitempty"`
	// Tunnel describes the cloudflared container the agent runs, when there is one.
	Tunnel *TunnelInfo `json:"tunnel,omitempty"`
}

// TunnelInfo is what the agent knows about its cloudflared container.
type TunnelInfo struct {
	// Version is the connector version, for example "2025.11.1"; empty when it could not be read.
	Version string `json:"version,omitempty"`
	Image   string `json:"image,omitempty"`
}

// NetAddr is one IPv4 address of a device.
type NetAddr struct {
	Interface string `json:"interface"`
	Address   string `json:"address"`
	// Assigned is "dhcp", "static" or "" when the agent cannot tell.
	Assigned string `json:"assigned,omitempty"`
	// Prefix is the length of the subnet mask, for example 24.
	Prefix int `json:"prefix,omitempty"`
	// Gateway is the default route's next hop when this interface carries the default route.
	Gateway string `json:"gateway,omitempty"`
	// Wireless marks a Wi-Fi interface, which can't carry dedicated worker addresses.
	Wireless bool `json:"wireless,omitempty"`
}

// DedicatedIP gives a worker its own address on the device's LAN through a Docker macvlan
// network, so it can use any port (such as 80 and 443) whatever else the device runs.
type DedicatedIP struct {
	// Mode is "dhcp" (the agent leases the address with MAC) or "static" (IP is used as given).
	Mode string `json:"mode"`
	// IP is the fixed address in static mode. In DHCP mode it is empty in settings and filled in
	// on the way to the agent with the last address the worker leased.
	IP string `json:"ip,omitempty"`
	// MAC is the worker's own hardware address, generated once so a router can reserve an address for it.
	MAC string `json:"mac"`
	// Parent is the network interface to attach to; empty uses the one with the default route.
	Parent string `json:"parent,omitempty"`
	// HostIP, when set, gives the device itself a way to reach the worker: a macvlan "shim"
	// interface on the device with this spare address in the same subnet (macvlan otherwise
	// blocks traffic between a device and its own containers).
	HostIP string `json:"host_ip,omitempty"`
	// VLAN, when set (1-4094), puts the worker on that 802.1Q VLAN: the agent creates a tagged
	// sub-interface (for example eth0.1005) on Parent and uses it as the macvlan parent. The
	// switch port must carry the VLAN tagged and the router must serve it.
	VLAN int `json:"vlan,omitempty"`
	// Ports, when set, replace the project's ports for the copy on this address (for example 80:80
	// and 443:443 on one device and 8000 on another). Empty uses the project's ports.
	Ports []string `json:"ports,omitempty"`
	// Subnet (like 192.168.5.0/24) and Gateway describe the VLAN's network in static mode. In DHCP
	// mode they are learned from the DHCP server. Unused without a VLAN.
	Subnet  string `json:"subnet,omitempty"`
	Gateway string `json:"gateway,omitempty"`
}

// Dedicated IP modes.
const (
	DedicatedDHCP   = "dhcp"
	DedicatedStatic = "static"
)

// NetworkCheck is the agent's answer to a network.check command.
type NetworkCheck struct {
	Parent   string   `json:"parent,omitempty"`
	Subnet   string   `json:"subnet,omitempty"`
	Gateway  string   `json:"gateway,omitempty"`
	Wireless bool     `json:"wireless,omitempty"`
	Problems []string `json:"problems,omitempty"`
	// SuggestHostIP is a free address the device itself could use to reach the worker (the
	// "address for this device to reach the worker"), offered when none is set. It is only a suggestion.
	SuggestHostIP string `json:"suggest_host_ip,omitempty"`
	// VLANReachable says whether anything on the VLAN answered: "yes" (the gateway or the DHCP
	// server did), "no" (nothing did, so the VLAN may not be reaching the device) or "" when it
	// couldn't be told. Only set for a worker on a VLAN.
	VLANReachable string `json:"vlan_reachable,omitempty"`
	// Offer is the address DHCP would hand out right now (dhcp mode only).
	Offer string `json:"offer,omitempty"`
	// Notes are things worth knowing that don't block the worker.
	Notes []string `json:"notes,omitempty"`
}

// Agent capabilities.
// NoBackupMarker is a file the server places in its backup folder; agents leave a folder holding it
// out of container backups.
const NoBackupMarker = ".orchestrator-no-backup"

const (
	CapBackup           = "backup"             // container.backup and container.restore
	CapUpdate           = "update"             // agent.update
	CapProxy            = "proxy"              // proxy.sync
	CapProxyDNS         = "proxy-dns"          // proxy.sync with a Cloudflare token (DNS challenge)
	CapPreview          = "preview"            // container.remove
	CapStream           = "stream"             // live log following and container shells
	CapInspect          = "inspect"            // container.inspect, used to adopt existing containers
	CapFileStore        = "filestore"          // file.store, receives server backups
	CapCompose          = "compose"            // deploy of a Compose stack, and stack.start/stop/down
	CapTunnel           = "tunnel"             // tunnel.sync, runs cloudflared
	CapTunnelUpdate     = "tunnel-update"      // tunnel.sync with Args["update"]: pull the newest cloudflared and recreate; reports its version
	CapDiscover         = "discover"           // finds the server again by itself when its address changes
	CapServerInstall    = "server-install"     // server.install, sets up a new Orchestrator server from a transfer bundle
	CapDedicatedDHCPRaw = "dedicated-dhcp-raw" // DHCP replies read on a packet socket, with a detailed "no answer" error
	CapDedicatedRelease = "dedicated-release"  // stopping or removing a worker also removes the device's access address
	CapDedicatedVLAN    = "dedicated-vlan"     // DedicatedIP.VLAN (an older agent would ignore it and use the untagged network)
	CapDedicatedIP      = "dedicated-ip"       // deploy with DeploySpec.Dedicated, and network.check
	CapDisks            = "disks"              // disk.list/format/partition/mount/unmount and share.mount/unmount
	CapLimits           = "limits"             // container.update, and DeploySpec.MaxRetries / MemoryMB
	CapImages           = "images"             // image.flash, boot.get and boot.set (gold images, Raspberry Pi boot order)
	CapFence            = "fence"              // refuses servers whose epoch is lower than one it has seen, and reports them
	CapStats            = "stats"              // sends per-container StatsReport with heartbeats, and proxy metrics
	CapHWMetrics        = "hw-metrics"         // heartbeats carry disk and network rates, load, swap, clock and Raspberry Pi power flags
	CapPower            = "power"              // device.power: graceful shutdown or restart of the whole machine
	CapDockerGC         = "docker-gc"          // docker.report and docker.prune, and DeploySpec.KeepImages (old worker images trimmed after a deploy)
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
	// TempC is the hottest thermal zone in °C; 0 when the device has no sensor (or the agent predates it).
	TempC float64 `json:"temp_c,omitempty"`
	// Sensors lists every temperature sensor the device exposes (SoC, NVMe, and so on).
	Sensors []TempSensor `json:"sensors,omitempty"`

	// IOWaitPercent is the share of CPU time spent waiting for disk or network I/O since the last report.
	IOWaitPercent float64 `json:"iowait_percent,omitempty"`
	// SwapUsed and SwapTotal are in bytes; SwapTotal is 0 when the device has no swap.
	SwapUsed  uint64 `json:"swap_used,omitempty"`
	SwapTotal uint64 `json:"swap_total,omitempty"`
	// OOMKills counts processes the kernel killed for running out of memory since boot.
	OOMKills uint64 `json:"oom_kills,omitempty"`
	// ClockMHz is the current CPU clock, and MaxClockMHz the highest the kernel allows.
	ClockMHz    float64 `json:"clock_mhz,omitempty"`
	MaxClockMHz float64 `json:"max_clock_mhz,omitempty"`
	// Disks and Nets are per-device rates since the previous report. They are empty on the first report.
	Disks []DiskIO `json:"disks,omitempty"`
	Nets  []NetIO  `json:"nets,omitempty"`
	// Power is the Raspberry Pi firmware's power and throttling state; nil on other hardware.
	Power *PowerInfo `json:"power,omitempty"`
}

// DiskIO is the activity of one physical disk (for example mmcblk0 or nvme0n1).
type DiskIO struct {
	Name      string  `json:"name"`
	ReadBps   float64 `json:"read_bps"`
	WriteBps  float64 `json:"write_bps"`
	ReadIOPS  float64 `json:"read_iops"`
	WriteIOPS float64 `json:"write_iops"`
	// Busy is the share of time the disk had requests in flight (0-100).
	Busy float64 `json:"busy"`
}

// NetIO is the traffic of one network interface.
type NetIO struct {
	Name  string  `json:"name"`
	RxBps float64 `json:"rx_bps"`
	TxBps float64 `json:"tx_bps"`
	// Errors counts receive and transmit errors and dropped packets per second.
	Errors float64 `json:"errors,omitempty"`
	// Wireless interfaces carry the signal level in dBm (negative; closer to 0 is stronger).
	Wireless  bool `json:"wireless,omitempty"`
	SignalDBm int  `json:"signal_dbm,omitempty"`
}

// PowerInfo is the decoded Raspberry Pi get_throttled word. The Now flags describe the present;
// the Since flags stay set from the moment the condition first happened until the next reboot.
type PowerInfo struct {
	Raw               uint32 `json:"raw"`
	UnderVoltage      bool   `json:"under_voltage,omitempty"`
	FreqCapped        bool   `json:"freq_capped,omitempty"`
	Throttled         bool   `json:"throttled,omitempty"`
	TempLimit         bool   `json:"temp_limit,omitempty"`
	SinceUnderVoltage bool   `json:"since_under_voltage,omitempty"`
	SinceFreqCapped   bool   `json:"since_freq_capped,omitempty"`
	SinceThrottled    bool   `json:"since_throttled,omitempty"`
	SinceTempLimit    bool   `json:"since_temp_limit,omitempty"`
}

// DecodePower splits a get_throttled word into its flags.
func DecodePower(raw uint32) *PowerInfo {
	b := func(n uint) bool { return raw&(1<<n) != 0 }
	return &PowerInfo{Raw: raw,
		UnderVoltage: b(0), FreqCapped: b(1), Throttled: b(2), TempLimit: b(3),
		SinceUnderVoltage: b(16), SinceFreqCapped: b(17), SinceThrottled: b(18), SinceTempLimit: b(19)}
}

// TempSensor is one temperature reading.
type TempSensor struct {
	Name string  `json:"name"`
	C    float64 `json:"c"`
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
	// Volumes are the names of the named Docker volumes mounted in the container (not bind mounts).
	Volumes []string `json:"volumes,omitempty"`
	// Networks lists the Docker networks the container is attached to with its address and MAC on each.
	Networks []ContainerNet `json:"networks,omitempty"`
	// Unstable is set by the server (never the agent) when Docker has restarted the container
	// repeatedly in the last few minutes, even if it is "running" at this instant.
	Unstable bool `json:"unstable,omitempty"`
}

// ContainerNet is a container's attachment to one Docker network.
type ContainerNet struct {
	Name string `json:"name"`
	IP   string `json:"ip,omitempty"`
	MAC  string `json:"mac,omitempty"`
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
	// Setup is the ID of a setup plan made in the Add device wizard; the server applies it once.
	Setup string   `json:"setup,omitempty"`
	Host  HostInfo `json:"host"`
}

type RegisterResponse struct {
	DeviceID   string `json:"device_id"`
	AgentToken string `json:"agent_token"`
	// ServerKey is the server's public identity key (base64 Ed25519). An agent keeps it so it
	// can recognise this server again at a new address.
	ServerKey string `json:"server_key,omitempty"`
}

// IdentifyResponse answers GET /api/agent/identify?nonce=...: the server signs
// IdentifyMessage(nonce) with its identity key.
type IdentifyResponse struct {
	ServerKey string `json:"server_key"`
	Signature string `json:"signature"`
}

// IdentifyMessage is what a server signs to prove who it is to an agent looking for it.
func IdentifyMessage(nonce string) []byte { return []byte("orchestrator-identify:" + nonce) }

type HeartbeatRequest struct {
	Host       HostInfo    `json:"host"`
	Metrics    Metrics     `json:"metrics"`
	Containers []Container `json:"containers"`
	// Stats is set when the agent has taken a new per-container sample since the last heartbeat.
	Stats *StatsReport `json:"stats,omitempty"`
	// Epoch is the highest server epoch the agent has seen (0 when none, or an older agent).
	Epoch int `json:"epoch,omitempty"`
	// StaleServers are servers the agent has refused lately because their epoch was lower.
	StaleServers []StaleServer `json:"stale_servers,omitempty"`
}

// StaleServer is a server the agent ignored because it reported an older epoch than the agent
// had already seen: typically an old active server that came back after a failover.
type StaleServer struct {
	URL   string    `json:"url"`
	Epoch int       `json:"epoch"`
	Seen  time.Time `json:"seen"`
}

// StatsReport is one per-container resource sample from a device. Counters (network, block
// I/O, throttling, proxy requests) are cumulative since the container or proxy started; the
// server turns them into rates.
type StatsReport struct {
	Time       int64            `json:"time"` // unix seconds
	Containers []ContainerStats `json:"containers"`
	// Volumes maps named volume to its size in bytes. It is only sent when re-measured
	// (about every 30 minutes), because measuring walks the volume's files.
	Volumes map[string]int64 `json:"volumes,omitempty"`
	// Proxy holds the device's Caddy proxy counters per site, when the proxy exposes them.
	Proxy []ProxyHostStats `json:"proxy,omitempty"`
}

// ContainerStats is one running container's resource use.
type ContainerStats struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// CPUPercent is percent of one core (200 means two full cores), measured since the
	// previous sample.
	CPUPercent float64 `json:"cpu"`
	MemUsed    uint64  `json:"mem"`
	MemLimit   uint64  `json:"mem_limit"` // the container's memory limit; 0 when it has none
	NetRx      uint64  `json:"net_rx"`
	NetTx      uint64  `json:"net_tx"`
	// HostNetwork is set for containers on the host network, whose traffic Docker cannot count.
	HostNetwork bool   `json:"host_network,omitempty"`
	BlkRead     uint64 `json:"blk_read"`
	BlkWrite    uint64 `json:"blk_write"`
	PIDs        uint64 `json:"pids"`
	// ThrottledNs is the total time the container was held back by its CPU limit.
	ThrottledNs uint64 `json:"throttled_ns,omitempty"`
	Restarts    int    `json:"restarts"`
	StartedAt   int64  `json:"started_at"`       // unix seconds
	Health      string `json:"health,omitempty"` // healthy, unhealthy, starting, or empty without a health check
	OOMKilled   bool   `json:"oom_killed,omitempty"`
}

// ProxyHostStats is Caddy's cumulative request counters for one site address.
type ProxyHostStats struct {
	Host string `json:"host"`
	// Codes counts requests by status class: "2xx", "3xx", "4xx", "5xx".
	Codes map[string]float64 `json:"codes"`
	// Buckets is the cumulative request duration histogram: upper bound in seconds to count.
	Buckets []ProxyBucket `json:"buckets"`
	// Bytes is the total response size sent.
	Bytes float64 `json:"bytes"`
}

// ProxyBucket is one cumulative histogram bucket.
type ProxyBucket struct {
	Le    float64 `json:"le"`
	Count float64 `json:"count"`
}

type HeartbeatResponse struct {
	Commands        []Command `json:"commands"`
	IntervalSeconds int       `json:"interval_seconds"`
	// Cancel lists ids of recently cancelled commands, so an agent can abort one it is running.
	Cancel []string `json:"cancel,omitempty"`
	// ServerKey is the server's public identity key; see RegisterResponse.
	ServerKey string `json:"server_key,omitempty"`
	// ServerURLs are the addresses the admin listed for agents (Settings > Agents). An agent
	// saves them and falls back to them when its own address stops answering.
	ServerURLs []string `json:"server_urls,omitempty"`
	// MoveTo is set by a server that has handed its role to another one (a server move). An
	// agent that can reach MoveTo switches to it and keeps this address as its way back.
	MoveTo string `json:"move_to,omitempty"`
}

// Command actions understood by the agent.
const (
	ActionStart   = "container.start"
	ActionStop    = "container.stop"
	ActionRestart = "container.restart"
	// ActionUpdate changes a running container's restart policy and memory limit in place
	// (docker update, no recreate): Args["restart"], ["retries"], ["memory_mb"] (0 = unchanged),
	// and ["stop"]="1" to stop it afterwards.
	ActionUpdate = "container.update"
	ActionLogs   = "container.logs"
	ActionDeploy = "deploy"
	// ActionBackup streams a container's config and data to the control plane; Args["backup_id"].
	ActionBackup = "container.backup"
	// ActionRestore writes a backup's data back into the existing container; Args["backup_id"].
	ActionRestore = "container.restore"
	// ActionAgentUpdate makes the agent replace its own binary with the control plane's build.
	ActionAgentUpdate = "agent.update"
	// ActionProxySync writes Args["caddyfile"] for the per-device Caddy proxy and (re)starts it.
	ActionProxySync = "proxy.sync"
	// ActionRemove force-removes the container named Target (a preview that is no longer wanted).
	// With Args["volumes"] == "delete" its named volumes go too, except any another container still mounts.
	ActionRemove = "container.remove"
	// ActionInspect reads a container's configuration and returns a ContainerSpec as JSON in the output.
	// It never changes the container.
	ActionInspect = "container.inspect"
	// ActionNetworkCheck validates a dedicated address before it is saved: Args["mode"], ["ip"],
	// ["mac"], ["parent"], ["service"]. The output is a NetworkCheck as JSON.
	ActionNetworkCheck = "network.check"
	// ActionFileStore downloads the server backup Args["name"] into the folder Args["dir"], keeping the newest Args["keep"].
	ActionFileStore = "file.store"
	// ActionServerInstall sets up a new Orchestrator server on this device from the transfer
	// bundle the current server offers (Args: name, image, port, bundle, password).
	ActionServerInstall = "server.install"
	// ActionStackStart, ActionStackStop and ActionStackDown act on every container of the Compose
	// stack named Target. Down removes the containers and networks; Args["volumes"] == "delete" also removes its volumes.
	ActionStackStart = "stack.start"
	ActionStackStop  = "stack.stop"
	ActionStackDown  = "stack.down"
	// ActionTunnelSync runs (or, with an empty Args["token"], removes) the device's cloudflared container.
	// With Args["update"] == "1" it also pulls the newest image and recreates the container if that changed it.
	ActionTunnelSync = "tunnel.sync"
	// Disk management (admin only, always sent by the server's disk API, never by the generic commands API).
	// ActionDiskList reports a DiskReport as JSON. It never changes anything.
	ActionDiskList = "disk.list"
	// ActionDiskFormat creates a filesystem on Args["device"] (fstype, label) once Args["confirm"] equals the device path.
	ActionDiskFormat = "disk.format"
	// ActionDiskPartition replaces the partition table of Args["device"] with GPT and Args["layout"] (MiB sizes, "rest").
	ActionDiskPartition = "disk.partition"
	// ActionDiskMount mounts Args["device"] at Args["mountpoint"]; Args["persist"] == "1" adds an fstab entry by UUID.
	ActionDiskMount = "disk.mount"
	// ActionDiskUnmount unmounts Args["mountpoint"] and drops its Orchestrator-written fstab entry.
	ActionDiskUnmount = "disk.unmount"
	// ActionShareMount mounts an SMB or NFS share (Args: kind, source, mountpoint, options, username, password, persist).
	ActionShareMount = "share.mount"

	// Rebuild from a pooled (gold) image. Admin only, sent by the server's image API, never by the generic commands API.
	// ActionImageFlash streams the server image Args["image_id"] (checked against Args["sha256"]) onto the whole disk
	// Args["device"] once Args["confirm"] equals the device path. With Args["enrol"] == "1" it also installs a first-boot
	// job that runs the agent installer with Args["join_token"] so the rebuilt machine registers itself.
	ActionImageFlash = "image.flash"
	// ActionBootGet reports the Raspberry Pi boot order (a BootInfo as JSON). It never changes anything.
	ActionBootGet = "boot.get"
	// ActionBootSet stages a new BOOT_ORDER (Args["order"], e.g. "0xf14") once Args["confirm"] equals it. It applies on the next reboot.
	ActionBootSet = "boot.set"
	// ActionDevicePower shuts the machine down (Args["mode"] == "poweroff") or restarts it ("reboot") once
	// Args["confirm"] equals the mode. Admin only, sent by the server's power API, never by the generic commands API.
	// The agent stops Docker (which stops every container gracefully and keeps their restart policies), syncs the
	// disks and then asks systemd. The result is reported before any of that starts, because the machine may be
	// the one the server runs on.
	ActionDevicePower = "device.power"
)

// Image is a gold image stored on the server: a raw disk image, optionally compressed.
type Image struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Arch      string    `json:"arch,omitempty"` // amd64, arm64, armv7 or empty when unknown
	Notes     string    `json:"notes,omitempty"`
	Format    string    `json:"format"` // raw, gz, xz or zst
	Size      int64     `json:"size"`   // bytes stored on the server
	SHA256    string    `json:"sha256"` // of the stored bytes
	CreatedBy string    `json:"created_by,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// BootInfo is the boot configuration of a Raspberry Pi.
type BootInfo struct {
	Supported bool   `json:"supported"`
	Reason    string `json:"reason,omitempty"` // why not, when Supported is false
	Model     string `json:"model,omitempty"`
	Order     string `json:"order,omitempty"`  // BOOT_ORDER as stored, e.g. "0xf41"
	Config    string `json:"config,omitempty"` // the whole EEPROM configuration
}

// DiskReport is what a device reports for disk.list.
type DiskReport struct {
	Disks  []Disk      `json:"disks"`
	Shares []DiskShare `json:"shares"`
	// Board is the device model (for example "Raspberry Pi Compute Module 5"), when the system reports one.
	Board string `json:"board,omitempty"`
}

// Disk is a block device (a whole disk or loop device) with its partitions.
type Disk struct {
	Path       string `json:"path"`
	Kind       string `json:"kind"` // disk, loop
	SizeBytes  int64  `json:"size_bytes"`
	Model      string `json:"model,omitempty"`
	Serial     string `json:"serial,omitempty"`
	Transport  string `json:"transport,omitempty"`
	Removable  bool   `json:"removable,omitempty"`
	Rotational bool   `json:"rotational,omitempty"`
	// Medium is "emmc" or "sd" for MMC-bus devices (from the kernel), empty when unknown.
	Medium      string   `json:"medium,omitempty"`
	BackingFile string   `json:"backing_file,omitempty"`
	Volume      Volume   `json:"volume"`
	Partitions  []Volume `json:"partitions,omitempty"`
	// Protected is why Orchestrator will not format, partition or unmount this disk (the system disk, or
	// something is mounted or otherwise using it). Empty means it is free to use.
	Protected string `json:"protected,omitempty"`
	// System is set when the device holds the operating system, Docker or the agent.
	System bool `json:"system,omitempty"`
}

// Volume is a filesystem (or the lack of one) on a disk or partition.
type Volume struct {
	Path       string `json:"path"`
	SizeBytes  int64  `json:"size_bytes"`
	FSType     string `json:"fstype,omitempty"`
	Label      string `json:"label,omitempty"`
	UUID       string `json:"uuid,omitempty"`
	Mountpoint string `json:"mountpoint,omitempty"`
	UsedBytes  int64  `json:"used_bytes,omitempty"`
	Persistent bool   `json:"persistent,omitempty"` // has an Orchestrator-written fstab entry
	// Protected is why this volume can't be formatted or unmounted from here.
	Protected string `json:"protected,omitempty"`
}

// DiskShare is a mounted SMB or NFS share.
type DiskShare struct {
	Source     string `json:"source"`
	Mountpoint string `json:"mountpoint"`
	FSType     string `json:"fstype"`
	SizeBytes  int64  `json:"size_bytes,omitempty"`
	UsedBytes  int64  `json:"used_bytes,omitempty"`
	Persistent bool   `json:"persistent,omitempty"`
}

// ContainerSpec is a running container's configuration, translated into what a DeploySpec can
// express. Anything the translation cannot carry over is named in Unsupported.
type ContainerSpec struct {
	Name       string            `json:"name"`
	Image      string            `json:"image"`
	Env        []string          `json:"env,omitempty"`
	Command    []string          `json:"command,omitempty"`
	Ports      []string          `json:"ports,omitempty"`
	Volumes    []string          `json:"volumes,omitempty"`
	Labels     map[string]string `json:"labels,omitempty"`
	Restart    string            `json:"restart,omitempty"`
	MaxRetries int               `json:"max_retries,omitempty"`
	MemoryMB   int               `json:"memory_mb,omitempty"`
	Network    string            `json:"network,omitempty"`
	// Unsupported lists settings that a redeploy would lose (privileged mode, devices, limits, ...).
	Unsupported []string `json:"unsupported,omitempty"`
}

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
	// CommandCancelled is a command the user gave up on; a running agent is told to abort it.
	CommandCancelled = "cancelled"
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
	// MaxRetries caps restarts for the on-failure policy (0 = Docker's default, unlimited).
	MaxRetries int `json:"max_retries,omitempty"`
	// MemoryMB is a hard memory limit in MiB (swap is limited to the same); 0 means no limit.
	MemoryMB int `json:"memory_mb,omitempty"`
	// Build, when set, builds Image on the device instead of pulling it.
	Build *BuildSpec `json:"build,omitempty"`
	// Labels are added to the container.
	Labels map[string]string `json:"labels,omitempty"`
	// Volumes are Docker-style mounts: a named volume or host path, a colon, the container path, optionally ":ro".
	Volumes []string `json:"volumes,omitempty"`
	// Network is a network name or "host"; empty is Docker's default bridge.
	Network string `json:"network,omitempty"`
	// Dedicated, when set, attaches the container to the device's LAN with its own address. Network
	// must be empty and published host ports are not used: the container listens on that address directly.
	Dedicated *DedicatedIP `json:"dedicated,omitempty"`
	// Compose, when set, makes this a Compose stack: Service is the stack (project) name and Image is unused.
	Compose *ComposeSpec `json:"compose,omitempty"`
	// ImageDigest is the content digest (sha256:...) the pulled Image resolved to, recorded after
	// a successful deployment so a rollback can pull exactly that version while the registry has it.
	ImageDigest string `json:"image_digest,omitempty"`
	// KeepImages, when above zero, trims older images of this worker's own build after a
	// successful deploy, leaving the newest KeepImages (the running one is never removed).
	KeepImages int `json:"keep_images,omitempty"`
	// Reuse lets the agent skip the build when Image already exists on the device (rollbacks).
	Reuse bool `json:"reuse,omitempty"`
	// Cloudflare, when set, makes this a deployment to Cloudflare Pages or Workers. The control
	// plane builds and uploads it itself; no agent or device is involved and Service is the project name.
	Cloudflare *CloudflareSpec `json:"cloudflare,omitempty"`
}

// Cloudflare deploy kinds.
const (
	CloudflarePages  = "pages"
	CloudflareWorker = "worker"
)

// CloudflareSpec describes one deployment to Cloudflare.
type CloudflareSpec struct {
	Kind string `json:"kind"` // CloudflarePages or CloudflareWorker
	// Name is the Pages project or Worker script the deployment goes to.
	Name string `json:"name"`
	// Repo and SHA say what to build; Context is the directory inside it.
	Repo    string `json:"repo"`
	SHA     string `json:"sha"`
	Context string `json:"context,omitempty"`
	// Branch is the git branch being deployed. For Pages, a branch other than ProductionBranch
	// becomes a preview deployment.
	Branch           string `json:"branch"`
	ProductionBranch string `json:"production_branch"`
	// Build holds the install and build commands (Start and Static are unused). Empty ones are detected.
	Build NodeSpec `json:"build"`
	// Output is the Pages directory to upload, or the Worker's entry module, relative to Context.
	Output string `json:"output,omitempty"`
	// CompatibilityDate is a Worker's compatibility date; empty means today.
	CompatibilityDate string `json:"compatibility_date,omitempty"`
}

// ComposeSpec is a Docker Compose file and the values its ${VAR} references are filled from.
type ComposeSpec struct {
	YAML string   `json:"yaml"`
	Env  []string `json:"env,omitempty"` // KEY=value
	// Remote, when set, is a git context (https://host/owner/repo.git#<sha>) the agent checks out
	// next to the stack, so services with build: and relative paths work. It may embed credentials,
	// so agents must never log it. YAML is still the compose file, as fetched at that commit.
	Remote string `json:"remote,omitempty"`
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
	case spec.Cloudflare != nil:
		return []string{StepCloning, StepInstalling, StepBuilding, StepUpload, StepVerify}
	case spec.Compose != nil && spec.Compose.Remote != "":
		return []string{StepInit, StepCloning, StepPulling, StepDeploy, StepVerify}
	case spec.Compose != nil:
		return []string{StepInit, StepPulling, StepDeploy, StepVerify}
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
	StepInit       = "Initialising"
	StepPulling    = "Pulling image"
	StepCloning    = "Cloning"
	StepInstalling = "Installing"
	StepBuilding   = "Building"
	StepUpload     = "Uploading"
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

// LooksLikeComposeFile reports whether a build "Dockerfile" path is really a Docker
// Compose or stack file, which BuildKit would fail to parse as a Dockerfile.
func LooksLikeComposeFile(p string) bool {
	b := strings.ToLower(path.Base(strings.TrimSpace(p)))
	if !strings.HasSuffix(b, ".yml") && !strings.HasSuffix(b, ".yaml") {
		return false
	}
	return strings.Contains(b, "compose") || strings.Contains(b, "stack")
}

// Docker storage housekeeping. Orchestrator builds an image per commit and pulls new tags over
// old ones, so a device collects images; these types describe what is there and what a prune did.
const (
	// ActionDockerReport returns a DockerStorage as JSON and never changes anything.
	ActionDockerReport = "docker.report"
	// ActionDockerPrune removes unused images and build cache and returns a DockerPruneResult as
	// JSON. Args: keep (newest images per worker to keep), trim=1, dangling=1, unused=1,
	// build_cache=1, cache_hours (0 = all of it).
	ActionDockerPrune = "docker.prune"
)

// DockerImageVersion is one tagged image of a worker built on the device.
type DockerImageVersion struct {
	Tag       string `json:"tag"`
	ID        string `json:"id"`
	Size      int64  `json:"size"`
	Created   int64  `json:"created"`
	InUse     bool   `json:"in_use"`
	Removable bool   `json:"removable"` // a prune with the current keep setting would remove it
}

// DockerImageGroup is every built image of one worker, newest first.
type DockerImageGroup struct {
	Repo     string               `json:"repo"`
	Worker   string               `json:"worker"`
	Orphaned bool                 `json:"orphaned"` // no container of this name exists on the device
	Versions []DockerImageVersion `json:"versions"`
}

// DockerStorage is what a device's Docker engine holds.
type DockerStorage struct {
	RootDir        string             `json:"root_dir"`
	Keep           int                `json:"keep"`
	Images         int                `json:"images"`
	ImagesSize     int64              `json:"images_size"`
	LayersSize     int64              `json:"layers_size"` // shared layers counted once
	Dangling       int                `json:"dangling"`
	DanglingSize   int64              `json:"dangling_size"`
	Unused         int                `json:"unused"` // tagged images no container uses, built or pulled
	UnusedSize     int64              `json:"unused_size"`
	Workers        []DockerImageGroup `json:"workers"`
	BuildCache     int64              `json:"build_cache"`
	BuildCacheFree int64              `json:"build_cache_reclaimable"`
	Reclaimable    int64              `json:"reclaimable"` // dangling + removable worker images + reclaimable cache (an estimate)
}

// DockerPruneResult is what a prune did.
type DockerPruneResult struct {
	ImagesRemoved int      `json:"images_removed"`
	Reclaimed     int64    `json:"reclaimed"`
	Steps         []string `json:"steps"`
}
