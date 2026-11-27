# Rebuilding devices from a gold image

Goal: take a machine to a standard build and have it rejoin the device pool by itself.
This document says what is implemented, what is designed but not built, and the hard limits.

## Implemented

| Piece | What it does |
| --- | --- |
| Gold images | Admins upload a raw disk image (optionally `.gz`, `.xz`, `.zst`) to the server (`<db dir>/images`). SHA-256 is recorded. |
| Write image | A device with a free disk downloads the image with its own token, decompresses it while writing, and verifies the checksum. Typical use: build a replacement SD/USB/NVMe disk on a working node, then move it into the machine to rebuild. |
| First-boot enrolment | Optional. A one-time systemd job is added to the new root filesystem. It fetches `/install.sh` and registers with the join token, then deletes itself and the token. Re-using an offline device's name keeps its identity. |
| Boot order | Reads and stages `BOOT_ORDER` on Pi 4, CM4, Pi 5 and CM5 with `rpi-eeprom-config`. Applies on next reboot. |

Safety: every action is admin-only, needs the device path (or boot order) typed back, is checked again on the device (system disk, mounted, LVM/RAID/LUKS refused, whole disks only), and is recorded in Activity with who asked. The join token is never stored in command history; it is added to the command only in the heartbeat response to the writing device.

## Bootstrap paths (designed, not built)

1. **Online node, pure software (built above).** Needs a second disk, or a node whose OS disk is removable and written from another node.
2. **Netboot service.** A server-side container on host networking, on the nodes' L2 network: dnsmasq in proxy-DHCP mode (no change to the existing DHCP server) plus TFTP/HTTP. A node whose boot order has network first receives a small recovery OS that streams the gold image onto its disk, installs the first-boot job and reboots. Must share the nodes' broadcast domain; it cannot work across routed networks without DHCP relay or a router change.
3. **One-time USB bootstrap for CM4/CM5 (usbboot).** The board is put in `nRPI_BOOT` mode and `rpiboot` loads a helper; `rpi-eeprom-config --apply` writes a config with `BOOT_ORDER=0xf12` (network first) plus an enrolment token. Afterwards the board netboots path 2 and adopts itself. A browser cannot do usbboot, so this runs from a helper container or CLI on a machine with USB access: either an operator's laptop, or an existing agent device wired to the board (a new `usbboot` agent capability, admin-only, typed confirmation of the board serial).
4. **Master image bundle.** The server mints a master enrolment token and a ZIP to extract onto a FAT32/MBR SD card or USB drive (GPT does not boot on the Pi ROM). Any board booted from it provisions itself and adopts into the pool. Needs: a separate token type that is revocable, expiring and use-limited (the current join token is a single shared secret), a list of devices each token enrolled, and a UI warning that every provisioned device leaves a credential on the card, so the card must be kept as secure as the token.

## Limits

- A dead node (no network, no storage) can only be recovered with a BMC/IPMI, `rpiboot` over USB, or physical access. Orchestrator cannot reach it.
- The running system disk cannot be rewritten by its own agent; rebuilding it needs path 2, 3 or 4 (boot from something else).
- Wrong boot-order values can leave a Pi unbootable until recovered over USB.
- The join token is currently one shared secret. Anything that writes it to a disk (this feature) puts it on that disk until first boot completes; paths 3 and 4 should use scoped, expiring tokens first.

## Not tested on real hardware

Only unit and API tests plus a build were run. Not run: real downloads/writes of multi-GiB images, xz/zst tools, `rpi-eeprom-config`, the first-boot job booting under systemd on Raspberry Pi OS, browser uploads of large files (the upload is a single streamed request; huge images are better copied onto the server directly).
