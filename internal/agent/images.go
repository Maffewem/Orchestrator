//go:build linux

package agent

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strings"

	"github.com/Maffewem/Orchestrator/internal/api"
)

var imageIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{6,64}$`)

func (a *Agent) imageAction(ctx context.Context, c api.Command) (string, error) {
	switch c.Action {
	case api.ActionImageFlash:
		return a.imageFlash(ctx, c.Args)
	case api.ActionBootGet:
		b, err := json.Marshal(bootInfo(ctx))
		return string(b), err
	case api.ActionBootSet:
		return bootSet(ctx, c.Args)
	}
	return "", fmt.Errorf("unsupported action %q", c.Action)
}

// imageFlash writes a gold image from the server onto a whole disk that is not in use.
// The target is re-checked against the device's live state exactly as format and partition are.
func (a *Agent) imageFlash(ctx context.Context, args map[string]string) (string, error) {
	dev := args["device"]
	if dev == "" || args["confirm"] != dev {
		return "", errors.New("not confirmed: the confirmation must equal the device path")
	}
	if !imageIDRe.MatchString(args["image_id"]) {
		return "", errors.New("invalid image id")
	}
	format := args["format"]
	switch format {
	case "raw", "gz", "xz", "zst":
	default:
		return "", fmt.Errorf("unsupported image format %q", format)
	}
	want := strings.ToLower(args["sha256"])
	if len(want) != 64 {
		return "", errors.New("the server did not send an image checksum")
	}
	rep, _, err := diskReport(ctx)
	if err != nil {
		return "", err
	}
	if err := guardWrite(rep, dev, api.ActionDiskPartition); err != nil {
		return "", err
	}
	disk, _, whole := findVolume(rep, dev)
	if !whole {
		return "", fmt.Errorf("%s is a partition; an image goes onto a whole disk", dev)
	}
	for _, p := range disk.Partitions {
		if p.Mountpoint != "" {
			return "", fmt.Errorf("%s is mounted at %s; unmount it first", p.Path, p.Mountpoint)
		}
	}
	enrol := args["enrol"] == "1"
	if enrol {
		// Fail before anything is written when the enrolment details are unusable.
		if err := validateEnrol(args["server"], args["join_token"], args["name"]); err != nil {
			return "", err
		}
	}

	u, err := url.Parse(a.server())
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(u.String(), "/")+"/api/agent/images/"+args["image_id"], nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+a.state.AgentToken)
	resp, err := (&http.Client{}).Do(req) // no overall timeout: images are large and links slow
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if err := a.fence(resp); err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("could not download the image (HTTP %d)", resp.StatusCode)
	}

	out, err := os.OpenFile(dev, os.O_WRONLY|os.O_EXCL, 0)
	if err != nil {
		return "", fmt.Errorf("could not open %s: %w", dev, err)
	}
	defer out.Close()

	h := sha256.New()
	body := io.TeeReader(resp.Body, h)
	var raw io.Reader = body
	var finish func() error
	switch format {
	case "gz":
		gz, err := gzip.NewReader(body)
		if err != nil {
			return "", fmt.Errorf("image is not valid gzip: %w", err)
		}
		raw = gz
	case "xz", "zst":
		tool := map[string]string{"xz": "xz", "zst": "zstd"}[format]
		cmd := exec.CommandContext(ctx, tool, "-dc")
		cmd.Stdin = body
		pipe, err := cmd.StdoutPipe()
		if err != nil {
			return "", err
		}
		if err := cmd.Start(); err != nil {
			return "", fmt.Errorf("%s is needed to read this image and could not be started: %w", tool, err)
		}
		raw = pipe
		finish = cmd.Wait
	}
	n, err := copyLimited(out, raw, disk.SizeBytes)
	if finish != nil {
		if werr := finish(); err == nil {
			err = werr
		}
	}
	if err != nil {
		return "", fmt.Errorf("writing %s failed after %s: %w. The disk is now in an unknown state; flash it again", dev, humanBytes(n), err)
	}
	if err := out.Sync(); err != nil {
		return "", err
	}
	io.Copy(io.Discard, body) // anything after the compressed stream still counts towards the checksum
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return "", fmt.Errorf("the downloaded image failed its checksum, so %s holds a damaged copy and must be flashed again", dev)
	}
	out.Close()
	if _, err := diskRun(ctx, "", "partprobe", dev); err != nil {
		diskRun(ctx, "", "partx", "-u", dev)
	}
	diskRun(ctx, "", "udevadm", "settle")
	msg := fmt.Sprintf("Wrote %s to %s and verified the download", humanBytes(n), dev)
	if enrol {
		if err := enrolDisk(ctx, dev, args); err != nil {
			return msg, fmt.Errorf("the image is written but first-boot registration could not be added: %w", err)
		}
		msg += "; the machine will register itself on first boot"
	}
	return msg, nil
}

// enrolDisk mounts the new root filesystem (the largest ext4 partition) and adds the first-boot job.
func enrolDisk(ctx context.Context, dev string, args map[string]string) error {
	rep, _, err := diskReport(ctx)
	if err != nil {
		return err
	}
	disk, _, _ := findVolume(rep, dev)
	if disk == nil {
		return errors.New("the disk is not visible after writing")
	}
	var root string
	var best int64
	for _, p := range disk.Partitions {
		if p.FSType == "ext4" && p.SizeBytes > best {
			root, best = p.Path, p.SizeBytes
		}
	}
	if root == "" {
		return errors.New("no ext4 root partition found on the image")
	}
	mnt, err := os.MkdirTemp("", "orch-enrol-")
	if err != nil {
		return err
	}
	defer os.Remove(mnt)
	if _, err := diskRun(ctx, "", "mount", "-t", "ext4", root, mnt); err != nil {
		return err
	}
	ierr := installFirstBoot(mnt, args["server"], args["join_token"], args["name"])
	if _, err := diskRun(ctx, "", "umount", mnt); err != nil && ierr == nil {
		ierr = err
	}
	return ierr
}

// copyLimited copies src to dst and fails if more than max bytes would be written.
func copyLimited(dst io.Writer, src io.Reader, max int64) (int64, error) {
	buf := make([]byte, 4<<20)
	var total int64
	for {
		nr, rerr := src.Read(buf)
		if nr > 0 {
			if total+int64(nr) > max {
				return total, fmt.Errorf("the image is larger than the disk (%s)", humanBytes(max))
			}
			nw, werr := dst.Write(buf[:nr])
			total += int64(nw)
			if werr != nil {
				return total, werr
			}
		}
		if rerr == io.EOF {
			return total, nil
		}
		if rerr != nil {
			return total, rerr
		}
	}
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// bootInfo reads the Raspberry Pi boot configuration. Other hardware reports it is not supported.
func bootInfo(ctx context.Context) api.BootInfo {
	model := readTrim("/proc/device-tree/model")
	info := api.BootInfo{Model: model}
	if !strings.Contains(model, "Raspberry Pi") {
		info.Reason = "Boot order is only managed on Raspberry Pi models with a configurable bootloader (Pi 4, CM4, Pi 5, CM5)."
		return info
	}
	cfg, err := diskRun(ctx, "", "rpi-eeprom-config")
	if err != nil {
		info.Reason = "Could not read the bootloader configuration: " + err.Error() + ". Pi 3 and older have no EEPROM; on others install rpi-eeprom."
		return info
	}
	info.Supported = true
	info.Config = cfg
	info.Order = parseBootOrder(cfg)
	return info
}

// bootSet stages a new BOOT_ORDER. rpi-eeprom-config --apply makes it take effect on the next reboot.
func bootSet(ctx context.Context, args map[string]string) (string, error) {
	order := strings.ToLower(args["order"])
	if !bootOrderValid.MatchString(order) {
		return "", fmt.Errorf("%q is not a valid boot order", args["order"])
	}
	if args["confirm"] != order {
		return "", errors.New("not confirmed: the confirmation must equal the boot order")
	}
	info := bootInfo(ctx)
	if !info.Supported {
		return "", errors.New(info.Reason)
	}
	f, err := os.CreateTemp("", "bootconf-")
	if err != nil {
		return "", err
	}
	defer os.Remove(f.Name())
	if _, err := f.WriteString(withBootOrder(info.Config, order)); err != nil {
		return "", err
	}
	f.Close()
	if out, err := diskRun(ctx, "", "rpi-eeprom-config", "--apply", f.Name()); err != nil {
		return out, err
	}
	return "Boot order set to " + order + ". It takes effect after the next reboot.", nil
}
