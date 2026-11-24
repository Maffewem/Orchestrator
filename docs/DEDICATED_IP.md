# Dedicated address per worker (Beta)

A worker can have its own IP and MAC address on the device's network. That lets it use ports such as 80 and 443 even when the device (or another worker) already holds them: the worker doesn't publish ports on the device at all, so there is nothing to clash with.

It uses a Docker **macvlan** network on the device's wired interface. To the rest of the network the worker looks like a separate machine.

## Turning it on

Project settings, Runtime, **Own address on the network** (administrators only, because it puts the worker directly on your LAN). Choose:

- **DHCP**: the agent leases an address from your router's DHCP server under the worker's own MAC address. The MAC is generated once and kept, so in your router you can reserve an address for it and the worker always gets the same IP. The leased address is shown on the worker's page and in Workers & stacks. The agent renews the lease while the worker exists.
- **Static**: you type the IP address.

**Check on the network** asks the device before you save: whether the interface is wired, whether a static address is inside the subnet, not the gateway, not the device's own address and not answering on the LAN (arping, falling back to ping), or which address DHCP would offer. The server also refuses an address or MAC another worker or device already uses. The same checks run again at deploy time.

List the worker's ports as `80:80` or just `80`. The host side of a mapping is ignored. Redeploy to apply a change.

## Putting a worker on another VLAN (Beta)

A worker can live on a different VLAN from the device while the device keeps using its single ethernet port. Under **Advanced**, enter a **VLAN ID** (1 to 4094). The agent creates an 802.1Q tagged sub-interface on the chosen network interface, named `<interface>.<id>`, and uses it as the macvlan parent. Example: VLAN ID `1005`, which carries the `192.168.5.0/24` network, gives `eth0.1005` and a worker address such as `192.168.5.20`. Different workers can use different VLANs on the same port.

What has to exist outside Orchestrator:

- **The switch port must carry the VLAN tagged.** It must be a trunk (or "tagged member") for that VLAN, in addition to whatever untagged network the device uses. Without it, nothing leaves the sub-interface and DHCP gets no answer.
- **The router must serve the VLAN.** For DHCP it needs a DHCP server on that VLAN; for any traffic outside the VLAN (including from other VLANs to the worker) it needs routing and firewall rules between the networks.

Addressing:

- **DHCP**: the agent asks on the VLAN under the worker's own MAC. The subnet and gateway come from the DHCP answer, not from the VLAN ID. Reserve the MAC in the router to keep the address fixed.
- **Static**: enter the address, the **subnet mask** (a length like `24`, a mask like `255.255.255.0`, or the full network like `192.168.5.0/24`) and the **VLAN gateway** (for example `192.168.5.1`). The network is worked out from the address and the mask; it is never guessed from the VLAN ID. The address must be inside the subnet, not the network, broadcast or gateway address, and not used by another worker or device. **Check on the network** also probes the VLAN for an answer from that address.

**"Looks free" is not proof the VLAN works.** Nothing answering for an address is also what a VLAN that carries no traffic looks like, so the check also asks for the gateway you entered (or, if empty, the subnet's first address, where routers usually are). If it answers, the result says the VLAN is working. If nothing answers, a gateway you entered makes the check fail, and a guessed one only warns. A DHCP offer on the VLAN proves it works as well. This needs `arping` on the device; without it the check says it can't tell.

Details:

- The sub-interface is created when you check or deploy, brought up, and recreated when the agent starts. Docker also recreates it for its macvlan network (`orch-lan-eth0.1005`) when the Docker daemon starts, so restart-policy containers come back after a reboot.
- The device itself is not on the VLAN. As above, set **Address for this device to reach the worker** (a spare address on the VLAN's subnet) if Caddy or the Cloudflare Tunnel on this device must reach the worker. Each VLAN gets its own access interface (`orch-shimv1005`).
- **Port conflicts**: none, as for any worker with its own address, so the worker can take 80 and 443 on the VLAN. Domains and the tunnel work only through the address for this device to reach the worker.
- Agents older than this feature refuse a VLAN (an older agent would silently attach the worker to the untagged network). Update the agent first.
- **Moving the worker** to another device: that device's switch port must carry the VLAN too, which can't be checked from here. A static address keeps working because the subnet belongs to the VLAN, not the device.
- **Removing it**: when no worker uses a VLAN any more (the worker moved to another VLAN, the feature was turned off and redeployed, or the worker was deleted), the agent deletes the macvlan network and the sub-interface, and with it the access interface on that VLAN. A sub-interface that already carries an IPv4 address (one you set up yourself) is left alone.
- The agent needs root and the `8021q` kernel module (loaded automatically by `ip link add ... type vlan` on Raspberry Pi OS and Ubuntu) and `ip` (iproute2).

### If DHCP on the VLAN gets no answer

The error lists what the agent saw: whether the sub-interface exists, its state and carrier, the reverse-path filter (`rp_filter`) setting, how many requests were sent and how many DHCP packets were seen on the interface. Replies are read through a packet socket on the sub-interface, because the kernel's reverse-path filter drops ordinary UDP replies from a router whose network the interface has no address in. "0 DHCP packets seen" means nothing came back on that VLAN: check that the UniFi (or other) network's VLAN ID is exactly the ID you entered, that DHCP is enabled for it and that the switch port carries it tagged. To test by hand on the device, create the interface with `ip link add link eth0 name eth0.<id> type vlan id <id>`, bring it up and watch `tcpdump -n -e -i eth0 vlan <id> and port 67 or port 68`.

## Replicas and per-copy settings (Beta)

Each copy of a worker (the main copy and every replica) has its own address settings: DHCP or static, IP, subnet mask, gateway, VLAN, network interface, device-side address, its own generated MAC, and its own list of ports ("Ports on this address", one per line). Use the copy switcher at the top of the worker page, then open Settings. Settings saved before this feature belong to the main copy.

- Two copies can never share an IP or a MAC address; saving is refused with the copy named.
- When a worker moves or fails over to another device, it uses that device's own copy settings, and the previous main settings are kept as that device's copy entry.
- The port-conflict check only compares ports on the same device and address.
- Saved settings only take effect at the next deploy. Until then the Overview marks the copy "Pending redeploy" and shows the address it is running at next to the configured one.
- Giving one worker several addresses is not supported.

## Removing it

Turn the switch off and redeploy. The worker goes back to the default bridge network and published ports, the DHCP lease is no longer renewed (it expires), and the device's route to the worker is removed.

## What changes for a worker with its own address

| Area | Behaviour |
| --- | --- |
| Port conflicts | Not checked against the device's ports. The worker listens on its own address. |
| Domains, Cloudflare Tunnel, production URL | Routed to the worker's own address and container port (the web port setting picks which), **if the worker has an "address for this device to reach the worker"**. Caddy and cloudflared run on the device, and Linux blocks the device from reaching its own macvlan workers, so the shim is what makes the route work. Without it the settings say why nothing is routed. With DHCP, reserve the MAC in the router: a changed lease needs a redeploy and a route resync. |
| Previews | Previews use normal device ports. They never take the worker's address. |
| Replicas | Replicas run on their own devices' normal ports. |
| Migration | The old copy is stopped first (an address can't be held twice) and started again if the move fails. A static address must fit the new device's subnet; DHCP works anywhere. |
| Backups | Unaffected (they cover volumes and settings). |
| Old agents | The option is refused until the device's agent is updated. |
| Wi-Fi | Not supported. Access points accept one MAC per client. The settings and the check say so. |

## The device can't reach its own macvlan workers

This is a Linux macvlan rule: the device and its macvlan containers can't talk to each other directly. Everything else on the network can. If something on the device must reach the worker, set **Address for this device to reach the worker** (Advanced). The agent then creates an `orch-shim` macvlan interface on the device with that spare address and a route to the worker, and rebuilds it when the agent restarts. Pick an unused address, outside your DHCP range.

The device holds this address itself, so nothing else may use it, yet your router doesn't know that: its DHCP server could hand the same address to another device. Choose an address **outside the router's DHCP pool**, or reserve it in the router. Orchestrator helps in two ways. **Suggest** (or the check) proposes an address near the top of the subnet or VLAN that nothing answers for and that isn't the gateway, the device, another worker or another known device; **Use suggestion** copies it into the form, and it is never saved without you saving the settings. A typed address gets the same check when you leave the field and again before saving: inside the subnet, not the gateway, network or broadcast address, not another worker's or device's address, and nothing answering for it on the network (the device's own existing access interface is allowed to answer). The check can't see your router's DHCP pool, so keeping the address outside it is up to you. Without it the deploy skips its reachability check (it can't test from the device).

## Requirements

Linux device with the agent running on the host (not as a container), a wired interface, and `ip` (iproute2) for the optional address for this device to reach the worker. `arping` or `ping` makes the static-address check reliable.

## Not tested live

Everything here is covered by unit tests (address and subnet checks, DHCP client against a fake server, settings validation, routing and migration rules), but none of it has run against a real router, Docker macvlan network or Pi yet. The VLAN option adds to that: creating the sub-interface, DHCP and static addressing on a tagged VLAN, the access interface per VLAN, restoring it after a reboot and cleanup are all untested against a real trunk port. First things to try: a DHCP worker on VLAN 1005 (192.168.5.0/24), then a DHCP worker on an ethernet Pi with a router reservation, a static worker, the address for this device to reach the worker, and moving a dedicated worker between devices.
