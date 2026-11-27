# Rebuilding devices from a gold image

Goal: take a machine to a standard build and have it rejoin the device pool by itself.
This document says what is implemented, what is designed but not built, and the hard limits.

## Implemented

| Piece | What it does |
| --- | --- |
| Gold images | Admins upload a raw disk image (optionally `.gz`, `.xz`, `.zst`) to the server (`<db dir>/images`). SHA-256 is recorded. |
| Write image | A device with a free disk downloads the image with its own token, decompresses it while writing, and verifies the checksum. Typical use: build a replacement SD/USB/NVMe disk on a working node, then move it into the machine to rebuild. |
| First-boot enrolment | Optional. A one-time systemd job is added to the new root filesystem. It fetches `/install.sh` and registers with the join token, then deletes itself and the token. Re-using an offline device's name keeps its identity. |
| Enrolment tokens | Scoped tokens (`enr_…`): expiring (max 30 days), limited uses (max 100), revocable, hashed at rest, listing which devices enrolled. Created in Settings; accepted by `/api/agent/register` and `install.sh --token`. Each first-boot rebuild mints its own one-use 48-hour token, handed to the writing device once. |
| Boot order | Reads and stages `BOOT_ORDER` on Pi 4, CM4, Pi 5 and CM5 with `rpi-eeprom-config`. Applies on next reboot. |

Safety: every action is admin-only, needs the device path (or boot order) typed back, is checked again on the device (system disk, mounted, LVM/RAID/LUKS refused, whole disks only), and is recorded in Activity with who asked. The enrolment token for a rebuild is never stored in command history; it is added to the command only in the heartbeat response to the writing device.

## Bootstrap paths (designed, not built)

1. **Online node, pure software (built above).** Needs a second disk, or a node whose OS disk is removable and written from another node.
2. **Netboot service.** A server-side container on host networking, on the nodes' L2 network: dnsmasq in proxy-DHCP mode (no change to the existing DHCP server) plus TFTP/HTTP. A node whose boot order has network first receives a small recovery OS that streams the gold image onto its disk, installs the first-boot job and reboots. Must share the nodes' broadcast domain; it cannot work across routed networks without DHCP relay or a router change.
3. **One-time USB bootstrap for CM4/CM5 (usbboot).** The board is put in `nRPI_BOOT` mode and `rpiboot` loads a helper; `rpi-eeprom-config --apply` writes a config with `BOOT_ORDER=0xf12` (network first) plus an enrolment token. Afterwards the board netboots path 2 and adopts itself. A browser cannot do usbboot, so this runs from a helper container or CLI on a machine with USB access: either an operator's laptop, or an existing agent device wired to the board (a new `usbboot` agent capability, admin-only, typed confirmation of the board serial).
4. **Master image bundle.** The server mints a master enrolment token and a ZIP to extract onto a FAT32/MBR SD card or USB drive (GPT does not boot on the Pi ROM). Any board booted from it provisions itself and adopts into the pool. Uses the enrolment tokens above (revocable, expiring, use-limited, with the list of devices each enrolled) and a UI warning that every provisioned device leaves a credential on the card, so the card must be kept as secure as the token.

## Limits

- A dead node (no network, no storage) can only be recovered with a BMC/IPMI, `rpiboot` over USB, or physical access. Orchestrator cannot reach it.
- The running system disk cannot be rewritten by its own agent; rebuilding it needs path 2, 3 or 4 (boot from something else).
- Wrong boot-order values can leave a Pi unbootable until recovered over USB.
- The shared join token still exists and still works; prefer enrolment tokens for anything that leaves a credential on media. A token on a card can enrol until it expires, is used up or is revoked.

## Not tested on real hardware

Only unit and API tests plus a build were run. Not run: real downloads/writes of multi-GiB images, xz/zst tools, `rpi-eeprom-config`, the first-boot job booting under systemd on Raspberry Pi OS, browser uploads of large files (the upload is a single streamed request; huge images are better copied onto the server directly).

## Netboot flow and choosing the OS (design)

**End to end**
1. The board's boot order is network first (set by Boot order, or written over USB once). It sends a DHCP request.
2. The existing DHCP server answers as normal. The netboot container (dnsmasq in proxy-DHCP mode, host networking, same L2 segment) adds only the boot server and file names, so no router change is needed.
3. The board fetches a small recovery OS by TFTP/HTTP (kernel, initramfs) built by Orchestrator. It carries the board serial and MAC.
4. The recovery OS calls the server with its serial and a short-lived enrolment token, asks "what should I become?", streams that gold image onto the target disk (same checksum and size checks as Write image), installs the first-boot job, switches the boot order back to local disk and reboots.
5. The new OS registers with its enrolment token; the server reuses the identity of an offline device with the same name.

**Choosing the OS**
- Image assignment is resolved in this order: a per-device assignment (matched by board serial or MAC, set before or after the first sighting), a per-group/label assignment, then the default image. Unknown boards are listed as "waiting for approval" with the serial and MAC; nothing is written until an admin approves (setting an assignment and pressing Rebuild).
- Safety: netboot rebuild never starts by itself on a device that already has a working OS. It runs only for a device an admin marked "rebuild on next network boot", or for an unknown board an admin approved. The image write has the same typed confirmation (the device serial) and audit entries.
- Not yet built: the assignment tables, the approval list, the recovery OS build, and the container.

**Limits**: the container must share the boards' broadcast domain; boards must have network in their boot order; Pi 4/CM4/Pi 5/CM5 only for network boot; the recovery OS and its drivers are untested.
