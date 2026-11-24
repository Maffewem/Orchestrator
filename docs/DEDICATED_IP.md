# Dedicated address per worker (Beta)

A worker can have its own IP and MAC address on the device's network. That lets it use ports such as 80 and 443 even when the device (or another worker) already holds them: the worker doesn't publish ports on the device at all, so there is nothing to clash with.

It uses a Docker **macvlan** network on the device's wired interface. To the rest of the network the worker looks like a separate machine.

## Turning it on

Project settings, Runtime, **Own address on the network** (administrators only, because it puts the worker directly on your LAN). Choose:

- **DHCP**: the agent leases an address from your router's DHCP server under the worker's own MAC address. The MAC is generated once and kept, so in your router you can reserve an address for it and the worker always gets the same IP. The leased address is shown on the worker's page and in Workers & stacks. The agent renews the lease while the worker exists.
- **Static**: you type the IP address.

**Check on the network** asks the device before you save: whether the interface is wired, whether a static address is inside the subnet, not the gateway, not the device's own address and not answering on the LAN (arping, falling back to ping), or which address DHCP would offer. The server also refuses an address or MAC another worker or device already uses. The same checks run again at deploy time.

List the worker's ports as `80:80` or just `80`. The host side of a mapping is ignored. Redeploy to apply a change.

## Removing it

Turn the switch off and redeploy. The worker goes back to the default bridge network and published ports, the DHCP lease is no longer renewed (it expires), and the device's route to the worker is removed.

## What changes for a worker with its own address

| Area | Behaviour |
| --- | --- |
| Port conflicts | Not checked against the device's ports. The worker listens on its own address. |
| Domains, Cloudflare Tunnel, production URL | Routed to the worker's own address and container port (the web port setting picks which), **if the worker has an "address for the device itself"**. Caddy and cloudflared run on the device, and Linux blocks the device from reaching its own macvlan workers, so the shim is what makes the route work. Without it the settings say why nothing is routed. With DHCP, reserve the MAC in the router: a changed lease needs a redeploy and a route resync. |
| Previews | Previews use normal device ports. They never take the worker's address. |
| Replicas | Replicas run on their own devices' normal ports. |
| Migration | The old copy is stopped first (an address can't be held twice) and started again if the move fails. A static address must fit the new device's subnet; DHCP works anywhere. |
| Backups | Unaffected (they cover volumes and settings). |
| Old agents | The option is refused until the device's agent is updated. |
| Wi-Fi | Not supported. Access points accept one MAC per client. The settings and the check say so. |

## The device can't reach its own macvlan workers

This is a Linux macvlan rule: the device and its macvlan containers can't talk to each other directly. Everything else on the network can. If something on the device must reach the worker, set **Address for the device itself** (Advanced). The agent then creates an `orch-shim` macvlan interface on the device with that spare address and a route to the worker, and rebuilds it when the agent restarts. Pick an unused address, outside your DHCP range. Without it the deploy skips its reachability check (it can't test from the device).

## Requirements

Linux device with the agent running on the host (not as a container), a wired interface, and `ip` (iproute2) for the optional device access address. `arping` or `ping` makes the static-address check reliable.

## Not tested live

Everything here is covered by unit tests (address and subnet checks, DHCP client against a fake server, settings validation, routing and migration rules), but none of it has run against a real router, Docker macvlan network or Pi yet. First things to try: a DHCP worker on an ethernet Pi with a router reservation, a static worker, the device access address, and moving a dedicated worker between devices.
