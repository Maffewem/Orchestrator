//go:build linux

package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"github.com/Maffewem/Orchestrator/internal/api"
)

// Disk management. Everything here runs as root on the device, so every action re-checks the live
// state of the device itself (never trusting the dashboard's last list) and refuses anything that
// is the system disk, mounted, or in use by LVM, RAID or encryption.

const fstabMarker = "# orchestrator"

var (
	fstabPath     = "/etc/fstab"
	credsDir      = "/etc/orchestrator-agent/credentials"
	mountInfoPath = "/proc/self/mountinfo"
)

// diskRun runs a tool and returns its trimmed combined output. It is a variable so tests can replace it.
var diskRun = func(ctx context.Context, stdin string, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	text := strings.TrimSpace(out.String())
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return text, fmt.Errorf("%s is not installed on this device", name)
		}
		if text != "" {
			return text, fmt.Errorf("%s failed: %s", name, text)
		}
		return text, fmt.Errorf("%s failed: %v", name, err)
	}
	return text, nil
}

// --- lsblk ---

type flexInt int64

func (f *flexInt) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "null" || s == "" {
		*f = 0
		return nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	*f = flexInt(n)
	return err
}

// flexBool accepts true/false as well as "1"/"0" (older lsblk prints strings).
type flexBool bool

func (f *flexBool) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	*f = flexBool(s == "true" || s == "1")
	return nil
}

type lsblkNode struct {
	Name        string      `json:"name"`
	Path        string      `json:"path"`
	Type        string      `json:"type"`
	Size        flexInt     `json:"size"`
	Model       *string     `json:"model"`
	Serial      *string     `json:"serial"`
	Tran        *string     `json:"tran"`
	FSType      *string     `json:"fstype"`
	Label       *string     `json:"label"`
	UUID        *string     `json:"uuid"`
	Mountpoints []*string   `json:"mountpoints"`
	Mountpoint  *string     `json:"mountpoint"`
	RM          flexBool    `json:"rm"`
	Rota        flexBool    `json:"rota"`
	Children    []lsblkNode `json:"children"`
}

func str(p *string) string {
	if p == nil {
		return ""
	}
	return strings.TrimSpace(*p)
}

func (n lsblkNode) mounts() []string {
	var out []string
	for _, m := range n.Mountpoints {
		if s := str(m); s != "" {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		if s := str(n.Mountpoint); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func parseLsblk(raw []byte) ([]lsblkNode, error) {
	var doc struct {
		Blockdevices []lsblkNode `json:"blockdevices"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("could not read lsblk output: %w", err)
	}
	return doc.Blockdevices, nil
}

func runLsblk(ctx context.Context) ([]lsblkNode, error) {
	cols := "NAME,PATH,TYPE,SIZE,MODEL,SERIAL,TRAN,FSTYPE,LABEL,UUID,MOUNTPOINTS,RM,ROTA"
	out, err := diskRun(ctx, "", "lsblk", "-J", "-b", "-o", cols)
	if err != nil && strings.Contains(out, "MOUNTPOINTS") {
		// util-linux before 2.37 only knows MOUNTPOINT.
		out, err = diskRun(ctx, "", "lsblk", "-J", "-b", "-o", strings.Replace(cols, "MOUNTPOINTS", "MOUNTPOINT", 1))
	}
	if err != nil {
		return nil, err
	}
	nodes, err := parseLsblk([]byte(out))
	if err != nil {
		return nil, err
	}
	for i := range nodes {
		probeLeaves(ctx, &nodes[i])
	}
	return nodes, nil
}

// probeLeaves reads the filesystem straight from the device. lsblk takes it from the udev
// database, which is empty on minimal systems and stale right after a format.
func probeLeaves(ctx context.Context, n *lsblkNode) {
	if len(n.Children) > 0 {
		for i := range n.Children {
			probeLeaves(ctx, &n.Children[i])
		}
		return
	}
	if n.Type != "disk" && n.Type != "loop" && n.Type != "part" {
		return
	}
	out, err := diskRun(ctx, "", "blkid", "-p", "-o", "export", n.Path)
	n.FSType, n.Label, n.UUID = nil, nil, nil
	if err != nil {
		return // no filesystem (blkid exits 2), or blkid is missing
	}
	for _, line := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		switch k {
		case "TYPE":
			n.FSType = &v
		case "LABEL":
			n.Label = &v
		case "UUID":
			n.UUID = &v
		}
	}
}

// --- system detection ---

type mountEntry struct{ Mountpoint, FSType, Source string }

func parseMountInfo(raw string) []mountEntry {
	var out []mountEntry
	for _, line := range strings.Split(raw, "\n") {
		f := strings.Fields(line)
		sep := -1
		for i, v := range f {
			if v == "-" {
				sep = i
				break
			}
		}
		if sep < 5 || len(f) < sep+3 {
			continue
		}
		out = append(out, mountEntry{Mountpoint: unescapeMount(f[4]), FSType: f[sep+1], Source: f[sep+2]})
	}
	return out
}

func unescapeMount(s string) string {
	r := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)
	return r.Replace(s)
}

// mountFor is the mount that holds path: the one with the longest matching mountpoint.
func mountFor(path string, mounts []mountEntry) string {
	best := "/"
	for _, m := range mounts {
		mp := m.Mountpoint
		if mp != "/" && (path == mp || strings.HasPrefix(path, mp+"/")) && len(mp) > len(best) {
			best = mp
		}
	}
	return best
}

// systemMountpoints are the mountpoints whose disk must never be touched: the operating system,
// boot partitions, Docker's data and the agent's own state.
func systemMountpoints(mounts []mountEntry) map[string]bool {
	set := map[string]bool{}
	for _, p := range []string{"/", "/boot", "/boot/firmware", "/boot/efi", "/usr", "/var", "/etc", "/home", "/opt", "/var/lib/docker", "/var/lib/orchestrator-agent", "/var/lib/containerd", "/var/lib/orchestrator"} {
		set[mountFor(p, mounts)] = true
	}
	for _, p := range []string{"/", "/boot", "/boot/firmware", "/boot/efi", "/usr", "/var", "/etc", "/home", "/opt"} {
		set[p] = true
	}
	return set
}

// blockedMountpoints can never be a mount target.
var blockedTargets = []string{"/proc", "/sys", "/dev", "/run", "/boot", "/etc", "/usr", "/bin", "/sbin", "/lib", "/lib64", "/var/lib/docker", "/var/lib/orchestrator-agent", "/root"}

// inUseFS are signatures of a device that something else already uses; formatting would destroy it.
var inUseFS = map[string]string{
	"LVM2_member":       "an LVM volume group",
	"linux_raid_member": "a software RAID array",
	"crypto_LUKS":       "an encrypted volume",
	"zfs_member":        "a ZFS pool",
}

// fstabState reads which mountpoints have an Orchestrator-written fstab entry.
func fstabEntries() map[string]bool {
	set := map[string]bool{}
	raw, err := os.ReadFile(fstabPath)
	if err != nil {
		return set
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if mp, ok := orchestratorLine(line); ok {
			set[mp] = true
		}
	}
	return set
}

func orchestratorLine(line string) (string, bool) {
	if !strings.HasSuffix(strings.TrimSpace(line), fstabMarker) {
		return "", false
	}
	f := strings.Fields(line)
	if len(f) < 2 || strings.HasPrefix(f[0], "#") {
		return "", false
	}
	return f[1], true
}

func buildDisks(nodes []lsblkNode, system map[string]bool, persistent map[string]bool, usage func(string) int64) []api.Disk {
	var disks []api.Disk
	for _, n := range nodes {
		if n.Type != "disk" && n.Type != "loop" {
			continue
		}
		mounts := n.mounts()
		if n.Size == 0 || (n.Type == "disk" && strings.HasPrefix(n.Name, "zram")) || strings.HasPrefix(n.Name, "ram") {
			continue
		}
		if len(mounts) == 1 && mounts[0] == "[SWAP]" && len(n.Children) == 0 {
			continue
		}
		d := api.Disk{
			Path: n.Path, Kind: n.Type, SizeBytes: int64(n.Size), Model: str(n.Model), Serial: str(n.Serial),
			Transport: str(n.Tran), Removable: bool(n.RM), Rotational: bool(n.Rota),
		}
		d.Volume = volumeOf(n, system, persistent, usage)
		var sys, busy bool
		var reasons []string
		walk(n, func(v lsblkNode) {
			for _, m := range v.mounts() {
				if system[m] || m == "[SWAP]" {
					sys = true
				} else {
					busy = true
					reasons = append(reasons, "mounted at "+m)
				}
			}
			if why, ok := inUseFS[str(v.FSType)]; ok {
				busy = true
				reasons = append(reasons, "part of "+why)
			}
			if v.Type != "disk" && v.Type != "loop" && v.Type != "part" {
				busy = true // lvm, crypt, raid and similar sit on top of this disk
				reasons = append(reasons, "used by a "+v.Type+" device")
			}
		})
		for _, c := range n.Children {
			if c.Type == "part" {
				d.Partitions = append(d.Partitions, volumeOf(c, system, persistent, usage))
			}
		}
		switch {
		case sys:
			d.System = true
			d.Protected = "System disk: it holds the operating system, Docker data or the agent."
		case busy:
			d.Protected = "In use (" + strings.Join(uniq(reasons), ", ") + "). Unmount or release it first."
		}
		disks = append(disks, d)
	}
	sort.Slice(disks, func(i, j int) bool { return disks[i].Path < disks[j].Path })
	return disks
}

func uniq(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func walk(n lsblkNode, f func(lsblkNode)) {
	f(n)
	for _, c := range n.Children {
		walk(c, f)
	}
}

func volumeOf(n lsblkNode, system, persistent map[string]bool, usage func(string) int64) api.Volume {
	v := api.Volume{Path: n.Path, SizeBytes: int64(n.Size), FSType: str(n.FSType), Label: str(n.Label), UUID: str(n.UUID)}
	for _, m := range n.mounts() {
		if v.Mountpoint == "" {
			v.Mountpoint = m
		}
		if system[m] || m == "[SWAP]" {
			v.Protected = "System volume."
		}
	}
	// A volume that is not mounted itself but carries a system child (LVM on a partition).
	walk(n, func(c lsblkNode) {
		for _, m := range c.mounts() {
			if system[m] && v.Protected == "" {
				v.Protected = "System volume."
			}
		}
	})
	if v.Mountpoint != "" && v.Mountpoint != "[SWAP]" {
		v.UsedBytes = usage(v.Mountpoint)
		v.Persistent = persistent[v.Mountpoint]
	}
	return v
}

func statUsed(path string) int64 {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0
	}
	return int64(st.Blocks-st.Bfree) * int64(st.Bsize)
}

func statSize(path string) int64 {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0
	}
	return int64(st.Blocks) * int64(st.Bsize)
}

func readMounts() []mountEntry {
	raw, err := os.ReadFile(mountInfoPath)
	if err != nil {
		return nil
	}
	return parseMountInfo(string(raw))
}

// diskReport reads the live state of the device's disks and network shares.
func diskReport(ctx context.Context) (api.DiskReport, []mountEntry, error) {
	nodes, err := runLsblk(ctx)
	if err != nil {
		return api.DiskReport{}, nil, err
	}
	mounts := readMounts()
	persistent := fstabEntries()
	rep := api.DiskReport{Disks: buildDisks(nodes, systemMountpoints(mounts), persistent, statUsed), Shares: []api.DiskShare{}}
	if rep.Disks == nil {
		rep.Disks = []api.Disk{}
	}
	for _, m := range mounts {
		switch m.FSType {
		case "nfs", "nfs4", "cifs", "smb3":
			rep.Shares = append(rep.Shares, api.DiskShare{Source: m.Source, Mountpoint: m.Mountpoint, FSType: m.FSType, SizeBytes: statSize(m.Mountpoint), UsedBytes: statUsed(m.Mountpoint), Persistent: persistent[m.Mountpoint]})
		}
	}
	return rep, mounts, nil
}

// --- guards (pure, so they are unit tested) ---

var devicePath = regexp.MustCompile(`^/dev/[A-Za-z0-9_-]+(/[A-Za-z0-9_-]+)?$`)

func findVolume(rep api.DiskReport, dev string) (disk *api.Disk, vol *api.Volume, whole bool) {
	for i := range rep.Disks {
		d := &rep.Disks[i]
		if d.Path == dev {
			return d, &d.Volume, true
		}
		for j := range d.Partitions {
			if d.Partitions[j].Path == dev {
				return d, &d.Partitions[j], false
			}
		}
	}
	return nil, nil, false
}

// guardWrite says why dev can't be formatted or re-partitioned right now, or nil.
func guardWrite(rep api.DiskReport, dev, op string) error {
	if !devicePath.MatchString(dev) {
		return fmt.Errorf("%q is not a valid device path", dev)
	}
	d, v, whole := findVolume(rep, dev)
	if d == nil {
		return fmt.Errorf("%s is not a disk or partition this device can manage (refresh the list)", dev)
	}
	if d.System {
		return fmt.Errorf("refusing: %s is on the system disk %s", dev, d.Path)
	}
	if whole {
		if d.Protected != "" {
			return fmt.Errorf("refusing: %s", d.Protected)
		}
		if op == api.ActionDiskFormat && len(d.Partitions) > 0 {
			return fmt.Errorf("%s has partitions; format one of them, or re-partition the disk first", dev)
		}
		return nil
	}
	if op == api.ActionDiskPartition {
		return fmt.Errorf("%s is a partition; re-partition the whole disk %s instead", dev, d.Path)
	}
	if v.Protected != "" {
		return fmt.Errorf("refusing: %s", v.Protected)
	}
	if v.Mountpoint != "" {
		return fmt.Errorf("%s is mounted at %s; unmount it first", dev, v.Mountpoint)
	}
	if why, ok := inUseFS[v.FSType]; ok {
		return fmt.Errorf("%s is part of %s and can't be formatted from here", dev, why)
	}
	return nil
}

var mountpointRe = regexp.MustCompile(`^/[A-Za-z0-9._/-]+$`)

func checkMountpoint(mp string) error {
	if !mountpointRe.MatchString(mp) || filepath.Clean(mp) != mp || strings.Contains(mp, "..") {
		return fmt.Errorf("%q is not a valid mountpoint: use a plain absolute path such as /mnt/data (letters, digits, . _ - /)", mp)
	}
	for _, b := range blockedTargets {
		if mp == b || strings.HasPrefix(mp, b+"/") {
			return fmt.Errorf("%s is a protected location and can't be used as a mountpoint", mp)
		}
	}
	return nil
}

var optionsRe = regexp.MustCompile(`^[A-Za-z0-9_=,.:-]*$`)

// --- actions ---

func (a *Agent) diskAction(ctx context.Context, c api.Command) (string, error) {
	switch c.Action {
	case api.ActionDiskList:
		rep, _, err := diskReport(ctx)
		if err != nil {
			return "", err
		}
		b, err := json.Marshal(rep)
		return string(b), err
	case api.ActionDiskFormat:
		return diskFormat(ctx, c.Args)
	case api.ActionDiskPartition:
		return diskPartition(ctx, c.Args)
	case api.ActionDiskMount:
		return diskMount(ctx, c.Args)
	case api.ActionDiskUnmount:
		return diskUnmount(ctx, c.Args)
	case api.ActionShareMount:
		return shareMount(ctx, c.Args)
	}
	return "", fmt.Errorf("unsupported action %q", c.Action)
}

func diskFormat(ctx context.Context, args map[string]string) (string, error) {
	dev, fs, label := args["device"], args["fstype"], args["label"]
	if args["confirm"] != dev || dev == "" {
		return "", errors.New("not confirmed: the confirmation must equal the device path")
	}
	tools := map[string][]string{
		"ext4":  {"mkfs.ext4", "-F", "-q"},
		"xfs":   {"mkfs.xfs", "-f", "-q"},
		"exfat": {"mkfs.exfat"},
		"vfat":  {"mkfs.vfat"},
	}
	tool, ok := tools[fs]
	if !ok {
		return "", fmt.Errorf("unsupported filesystem %q (ext4, xfs, exfat or vfat)", fs)
	}
	if !regexp.MustCompile(`^[A-Za-z0-9_-]{0,12}$`).MatchString(label) {
		return "", errors.New("the label can have up to 12 letters, digits, - or _")
	}
	rep, _, err := diskReport(ctx)
	if err != nil {
		return "", err
	}
	if err := guardWrite(rep, dev, api.ActionDiskFormat); err != nil {
		return "", err
	}
	if _, err := exec.LookPath(tool[0]); err != nil {
		return "", fmt.Errorf("%s is not installed on this device (install the %s tools first)", tool[0], fs)
	}
	if _, err := diskRun(ctx, "", "wipefs", "-a", dev); err != nil {
		return "", err
	}
	cmd := append([]string{}, tool[1:]...)
	if label != "" {
		if fs == "vfat" {
			cmd = append(cmd, "-n", label)
		} else {
			cmd = append(cmd, "-L", label)
		}
	}
	cmd = append(cmd, dev)
	if _, err := diskRun(ctx, "", tool[0], cmd...); err != nil {
		return "", err
	}
	diskRun(ctx, "", "udevadm", "settle")
	return fmt.Sprintf("Formatted %s as %s", dev, fs), nil
}

func diskPartition(ctx context.Context, args map[string]string) (string, error) {
	dev, layout := args["device"], args["layout"]
	if args["confirm"] != dev || dev == "" {
		return "", errors.New("not confirmed: the confirmation must equal the device path")
	}
	rep, _, err := diskReport(ctx)
	if err != nil {
		return "", err
	}
	if err := guardWrite(rep, dev, api.ActionDiskPartition); err != nil {
		return "", err
	}
	d, _, _ := findVolume(rep, dev)
	script, err := sfdiskScript(layout, d.SizeBytes)
	if err != nil {
		return "", err
	}
	if _, err := diskRun(ctx, "", "wipefs", "-a", dev); err != nil {
		return "", err
	}
	if out, err := diskRun(ctx, script, "sfdisk", "--wipe", "always", dev); err != nil {
		return out, err
	}
	// Ask the kernel to re-read the table; which of these exists differs between devices.
	if _, err := diskRun(ctx, "", "partprobe", dev); err != nil {
		diskRun(ctx, "", "partx", "-u", dev)
		diskRun(ctx, "", "partx", "-a", dev)
	}
	diskRun(ctx, "", "udevadm", "settle")
	return "Created a new GPT partition table on " + dev, nil
}

// sfdiskScript turns "2048,rest" (MiB sizes, the last may be "rest") into an sfdisk script.
func sfdiskScript(layout string, diskBytes int64) (string, error) {
	if strings.TrimSpace(layout) == "" {
		layout = "rest"
	}
	parts := strings.Split(layout, ",")
	if len(parts) > 16 {
		return "", errors.New("at most 16 partitions")
	}
	var total int64
	var b strings.Builder
	b.WriteString("label: gpt\n")
	for i, p := range parts {
		p = strings.TrimSpace(p)
		if p == "rest" {
			if i != len(parts)-1 {
				return "", errors.New(`"rest" can only be the last partition`)
			}
			b.WriteString(",,L\n")
			continue
		}
		n, err := strconv.ParseInt(p, 10, 64)
		if err != nil || n < 1 {
			return "", fmt.Errorf("%q is not a size in MiB", p)
		}
		total += n << 20
		if total > diskBytes-(2<<20) {
			return "", errors.New("the partitions are larger than the disk")
		}
		fmt.Fprintf(&b, "size=%dMiB, type=L\n", n)
	}
	return b.String(), nil
}

func diskMount(ctx context.Context, args map[string]string) (string, error) {
	dev, mp, opts := args["device"], args["mountpoint"], args["options"]
	if !optionsRe.MatchString(opts) {
		return "", errors.New("invalid mount options")
	}
	if err := checkMountpoint(mp); err != nil {
		return "", err
	}
	rep, mounts, err := diskReport(ctx)
	if err != nil {
		return "", err
	}
	d, v, _ := findVolume(rep, dev)
	switch {
	case d == nil:
		return "", fmt.Errorf("%s is not a disk or partition this device can manage (refresh the list)", dev)
	case d.System || v.Protected != "":
		return "", fmt.Errorf("refusing: %s is part of the system disk", dev)
	case v.Mountpoint != "":
		return "", fmt.Errorf("%s is already mounted at %s", dev, v.Mountpoint)
	case v.FSType == "":
		return "", fmt.Errorf("%s has no filesystem; format it first", dev)
	}
	if _, bad := inUseFS[v.FSType]; bad || v.FSType == "swap" {
		return "", fmt.Errorf("%s holds %s and can't be mounted as a disk", dev, v.FSType)
	}
	for _, m := range mounts {
		if m.Mountpoint == mp {
			return "", fmt.Errorf("something is already mounted at %s", mp)
		}
	}
	if err := ensureEmptyDir(mp); err != nil {
		return "", err
	}
	src := dev
	if v.UUID != "" {
		src = "UUID=" + v.UUID
	}
	margs := []string{"-t", v.FSType}
	if opts != "" {
		margs = append(margs, "-o", opts)
	}
	margs = append(margs, src, mp)
	if _, err := diskRun(ctx, "", "mount", margs...); err != nil {
		return "", err
	}
	msg := fmt.Sprintf("Mounted %s at %s", dev, mp)
	if args["persist"] == "1" {
		fopts := "defaults,nofail"
		if opts != "" {
			fopts += "," + opts
		}
		if err := fstabSet(src, mp, v.FSType, fopts); err != nil {
			return msg, fmt.Errorf("mounted, but could not make it persistent: %w", err)
		}
		msg += " and added it to /etc/fstab (by UUID) so it mounts at boot"
	}
	return msg, nil
}

func ensureEmptyDir(mp string) error {
	if err := os.MkdirAll(mp, 0o755); err != nil {
		return err
	}
	ents, err := os.ReadDir(mp)
	if err != nil {
		return err
	}
	if len(ents) > 0 {
		return fmt.Errorf("%s is not empty; mounting there would hide its files", mp)
	}
	return nil
}

func diskUnmount(ctx context.Context, args map[string]string) (string, error) {
	mp := args["mountpoint"]
	if err := checkMountpoint(mp); err != nil {
		return "", err
	}
	rep, _, err := diskReport(ctx)
	if err != nil {
		return "", err
	}
	known := false
	for _, d := range rep.Disks {
		for _, v := range append([]api.Volume{d.Volume}, d.Partitions...) {
			if v.Mountpoint == mp {
				if v.Protected != "" || d.System {
					return "", fmt.Errorf("refusing: %s is part of the system disk", mp)
				}
				known = true
			}
		}
	}
	for _, s := range rep.Shares {
		if s.Mountpoint == mp {
			known = true
		}
	}
	if !known {
		return "", fmt.Errorf("%s is not a mounted disk or share that Orchestrator manages", mp)
	}
	if _, err := diskRun(ctx, "", "umount", mp); err != nil {
		return "", fmt.Errorf("%w (is something using it? stop the workers that use it first)", err)
	}
	msg := "Unmounted " + mp
	if fstabDrop(mp) {
		msg += " and removed its Orchestrator entry from /etc/fstab"
	}
	os.Remove(filepath.Join(credsDir, credName(mp)))
	return msg, nil
}

var (
	smbSource = regexp.MustCompile(`^//[A-Za-z0-9.\-\[\]:]+/[A-Za-z0-9._$/-]+$`)
	nfsSource = regexp.MustCompile(`^[A-Za-z0-9.\-\[\]:]+:/[A-Za-z0-9._/-]*$`)
	userRe    = regexp.MustCompile(`^[A-Za-z0-9._@\\-]{1,64}$`)
)

func credName(mp string) string { return strings.Trim(strings.ReplaceAll(mp, "/", "_"), "_") }

func shareMount(ctx context.Context, args map[string]string) (string, error) {
	kind, src, mp, opts := args["kind"], args["source"], args["mountpoint"], args["options"]
	if !optionsRe.MatchString(opts) {
		return "", errors.New("invalid mount options")
	}
	if err := checkMountpoint(mp); err != nil {
		return "", err
	}
	_, mounts, err := diskReport(ctx)
	if err != nil {
		return "", err
	}
	for _, m := range mounts {
		if m.Mountpoint == mp {
			return "", fmt.Errorf("something is already mounted at %s", mp)
		}
	}
	var fs string
	fopts := []string{"nofail", "_netdev"}
	switch kind {
	case "nfs":
		fs = "nfs"
		if !nfsSource.MatchString(src) {
			return "", errors.New("an NFS source looks like server:/export/path")
		}
	case "smb":
		fs = "cifs"
		if !smbSource.MatchString(src) {
			return "", errors.New("an SMB source looks like //server/share")
		}
		user, pass := args["username"], args["password"]
		if user == "" {
			fopts = append(fopts, "guest")
		} else {
			if !userRe.MatchString(user) || strings.ContainsAny(pass, "\n\r") {
				return "", errors.New("invalid username or password characters")
			}
			if err := os.MkdirAll(credsDir, 0o700); err != nil {
				return "", err
			}
			cf := filepath.Join(credsDir, credName(mp))
			if err := os.WriteFile(cf, []byte("username="+user+"\npassword="+pass+"\n"), 0o600); err != nil {
				return "", err
			}
			fopts = append(fopts, "credentials="+cf)
		}
	default:
		return "", errors.New(`kind must be "smb" or "nfs"`)
	}
	if opts != "" {
		fopts = append(fopts, opts)
	}
	if err := ensureEmptyDir(mp); err != nil {
		return "", err
	}
	joined := strings.Join(fopts, ",")
	if _, err := diskRun(ctx, "", "mount", "-t", fs, "-o", joined, src, mp); err != nil {
		os.Remove(filepath.Join(credsDir, credName(mp)))
		return "", err
	}
	msg := fmt.Sprintf("Mounted %s at %s", src, mp)
	if args["persist"] == "1" {
		if err := fstabSet(src, mp, fs, joined); err != nil {
			return msg, fmt.Errorf("mounted, but could not make it persistent: %w", err)
		}
		msg += " and added it to /etc/fstab"
	}
	return msg, nil
}

// --- fstab ---

// fstabSet adds (or replaces) the Orchestrator-marked entry for mp. Lines Orchestrator did not
// write are never touched. The first change keeps a copy of the original as fstab.orchestrator.bak.
func fstabSet(src, mp, fs, opts string) error {
	raw, err := os.ReadFile(fstabPath)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if _, serr := os.Stat(fstabPath + ".orchestrator.bak"); serr != nil && err == nil {
		os.WriteFile(fstabPath+".orchestrator.bak", raw, 0o644)
	}
	lines := dropLines(string(raw), mp)
	lines = append(lines, fmt.Sprintf("%s %s %s %s 0 2 %s", src, mp, fs, opts, fstabMarker))
	return writeFstab(strings.Join(lines, "\n") + "\n")
}

func fstabDrop(mp string) bool {
	raw, err := os.ReadFile(fstabPath)
	if err != nil {
		return false
	}
	lines := dropLines(string(raw), mp)
	if len(lines) == len(strings.Split(strings.TrimRight(string(raw), "\n"), "\n")) {
		return false
	}
	return writeFstab(strings.Join(lines, "\n")+"\n") == nil
}

func dropLines(raw, mp string) []string {
	var out []string
	for _, l := range strings.Split(strings.TrimRight(raw, "\n"), "\n") {
		if m, ok := orchestratorLine(l); ok && m == mp {
			continue
		}
		out = append(out, l)
	}
	if len(out) == 1 && out[0] == "" {
		return nil
	}
	return out
}

func writeFstab(content string) error {
	mode := os.FileMode(0o644)
	if st, err := os.Stat(fstabPath); err == nil {
		mode = st.Mode().Perm()
	}
	tmp := fstabPath + ".orchestrator.tmp"
	if err := os.WriteFile(tmp, []byte(content), mode); err != nil {
		return err
	}
	return os.Rename(tmp, fstabPath)
}
