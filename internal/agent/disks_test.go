//go:build linux

package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Maffewem/Orchestrator/internal/api"
)

const sampleLsblk = `{"blockdevices":[
 {"name":"sda","path":"/dev/sda","type":"disk","size":"500107862016","model":"Samsung SSD","serial":"S1","tran":"sata","fstype":null,"label":null,"uuid":null,"mountpoints":[null],"rm":"0","rota":false,
  "children":[
   {"name":"sda1","path":"/dev/sda1","type":"part","size":536870912,"fstype":"vfat","mountpoints":["/boot/firmware"]},
   {"name":"sda2","path":"/dev/sda2","type":"part","size":499000000000,"fstype":"ext4","mountpoints":["/"]}]},
 {"name":"sdb","path":"/dev/sdb","type":"disk","size":2000000000000,"model":"WD Elements","rm":true,"mountpoints":[null],
  "children":[
   {"name":"sdb1","path":"/dev/sdb1","type":"part","size":1000000000000,"fstype":"ext4","label":"media","uuid":"u1","mountpoints":["/mnt/media"]},
   {"name":"sdb2","path":"/dev/sdb2","type":"part","size":1000000000000,"fstype":"exfat","uuid":"u2","mountpoints":[null]}]},
 {"name":"sdc","path":"/dev/sdc","type":"disk","size":1000000000,"mountpoints":[null]},
 {"name":"sdd","path":"/dev/sdd","type":"disk","size":1000000000,"mountpoints":[null],
  "children":[{"name":"sdd1","path":"/dev/sdd1","type":"part","size":900000000,"fstype":"LVM2_member","mountpoints":[null],
    "children":[{"name":"vg-root","path":"/dev/mapper/vg-root","type":"lvm","size":800000000,"fstype":"ext4","mountpoints":["/mnt/lvm"]}]}]},
 {"name":"zram0","path":"/dev/zram0","type":"disk","size":1000,"mountpoints":["[SWAP]"]}
]}`

func sampleReport(t *testing.T) api.DiskReport {
	t.Helper()
	nodes, err := parseLsblk([]byte(sampleLsblk))
	if err != nil {
		t.Fatal(err)
	}
	mounts := []mountEntry{{Mountpoint: "/", FSType: "ext4", Source: "/dev/sda2"}, {Mountpoint: "/boot/firmware", FSType: "vfat", Source: "/dev/sda1"}}
	return api.DiskReport{Disks: buildDisks(nodes, systemMountpoints(mounts), map[string]bool{"/mnt/media": true}, func(string) int64 { return 5 })}
}

func TestBuildDisksClassifies(t *testing.T) {
	rep := sampleReport(t)
	got := map[string]api.Disk{}
	for _, d := range rep.Disks {
		got[d.Path] = d
	}
	if _, ok := got["/dev/zram0"]; ok {
		t.Error("zram swap should not be listed")
	}
	if !got["/dev/sda"].System || got["/dev/sda"].Protected == "" {
		t.Error("the disk holding / must be the protected system disk")
	}
	if got["/dev/sdb"].System || !strings.Contains(got["/dev/sdb"].Protected, "mounted at /mnt/media") {
		t.Errorf("sdb should be in use, not system: %+v", got["/dev/sdb"])
	}
	if !got["/dev/sdb"].Partitions[0].Persistent || got["/dev/sdb"].Partitions[0].UsedBytes != 5 {
		t.Error("persistent flag and usage should be reported for mounted volumes")
	}
	if got["/dev/sdc"].Protected != "" {
		t.Error("a blank disk is free")
	}
	if !strings.Contains(got["/dev/sdd"].Protected, "LVM") {
		t.Errorf("an LVM member disk is in use: %q", got["/dev/sdd"].Protected)
	}
}

func TestGuardWrite(t *testing.T) {
	rep := sampleReport(t)
	check := func(dev, op, want string) {
		t.Helper()
		err := guardWrite(rep, dev, op)
		if want == "" && err != nil {
			t.Errorf("%s %s: unexpected %v", op, dev, err)
		}
		if want != "" && (err == nil || !strings.Contains(err.Error(), want)) {
			t.Errorf("%s %s: want error containing %q, got %v", op, dev, want, err)
		}
	}
	check("/dev/sda", api.ActionDiskFormat, "system disk")
	check("/dev/sda", api.ActionDiskPartition, "system disk")
	check("/dev/sda2", api.ActionDiskFormat, "system disk")
	check("/dev/sdb1", api.ActionDiskFormat, "mounted") // mounted partition of a disk that has another mounted partition
	check("/dev/sdb", api.ActionDiskPartition, "In use")
	check("/dev/sdb2", api.ActionDiskFormat, "")
	check("/dev/sdb2", api.ActionDiskPartition, "is a partition")
	check("/dev/sdc", api.ActionDiskFormat, "")
	check("/dev/sdc", api.ActionDiskPartition, "")
	check("/dev/sdd1", api.ActionDiskFormat, "LVM")
	check("/dev/nope", api.ActionDiskFormat, "not a disk")
	check("/dev/sdc; rm -rf /", api.ActionDiskFormat, "not a valid device")
	check("../../etc/passwd", api.ActionDiskFormat, "not a valid device")
}

func TestCheckMountpoint(t *testing.T) {
	for _, ok := range []string{"/mnt/data", "/media/usb-1", "/srv/nas.share"} {
		if err := checkMountpoint(ok); err != nil {
			t.Errorf("%s should be allowed: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "mnt/data", "/", "/mnt/a b", "/mnt/../etc", "/etc", "/usr/local", "/boot/firmware", "/var/lib/docker/x", "/mnt/x;y", "/mnt//x"} {
		if err := checkMountpoint(bad); err == nil {
			t.Errorf("%q should be refused", bad)
		}
	}
}

func TestSfdiskScript(t *testing.T) {
	s, err := sfdiskScript("2048,rest", 10<<30)
	if err != nil || s != "label: gpt\nsize=2048MiB, type=L\n,,L\n" {
		t.Errorf("unexpected script %q (%v)", s, err)
	}
	for _, bad := range []string{"rest,100", "abc", "0", "999999999", "1,2,3,4,5,6,7,8,9,10,11,12,13,14,15,16,17"} {
		if _, err := sfdiskScript(bad, 10<<30); err == nil {
			t.Errorf("layout %q should be refused", bad)
		}
	}
}

func TestFstabKeepsForeignLines(t *testing.T) {
	dir := t.TempDir()
	fstabPath = filepath.Join(dir, "fstab")
	t.Cleanup(func() { fstabPath = "/etc/fstab" })
	orig := "# static file system information\nUUID=aaa / ext4 defaults 0 1\nUUID=bbb /mnt/mine ext4 defaults 0 2\n"
	os.WriteFile(fstabPath, []byte(orig), 0o644)
	if err := fstabSet("UUID=u1", "/mnt/media", "ext4", "defaults,nofail"); err != nil {
		t.Fatal(err)
	}
	if err := fstabSet("UUID=u1", "/mnt/media", "ext4", "defaults,nofail,ro"); err != nil { // replaces, not duplicates
		t.Fatal(err)
	}
	b, _ := os.ReadFile(fstabPath)
	if strings.Count(string(b), "/mnt/media") != 1 || !strings.HasPrefix(string(b), orig) || !strings.Contains(string(b), "ro 0 2 "+fstabMarker) {
		t.Errorf("unexpected fstab:\n%s", b)
	}
	if bak, _ := os.ReadFile(fstabPath + ".orchestrator.bak"); string(bak) != orig {
		t.Error("the original fstab should be kept as a backup")
	}
	if !fstabEntries()["/mnt/media"] || fstabEntries()["/mnt/mine"] {
		t.Error("only Orchestrator's own entries count as persistent")
	}
	// A hand-written entry is never removed, ours is.
	if fstabDrop("/mnt/mine") {
		t.Error("must not drop a line Orchestrator did not write")
	}
	if !fstabDrop("/mnt/media") {
		t.Error("should drop our entry")
	}
	if b, _ := os.ReadFile(fstabPath); string(b) != orig {
		t.Errorf("fstab should be back to the original:\n%s", b)
	}
}

func TestFormatNeedsConfirmation(t *testing.T) {
	if _, err := diskFormat(t.Context(), map[string]string{"device": "/dev/sdc", "fstype": "ext4", "confirm": "yes"}); err == nil || !strings.Contains(err.Error(), "not confirmed") {
		t.Errorf("want a confirmation error, got %v", err)
	}
}
