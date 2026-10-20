# Tailbridge

**Tailbridge** is an open-source companion to [Tailscale](https://tailscale.com/). It makes individual devices reachable across a LAN and a tailnet without installing Tailscale on every device or configuring routes on every client.

Status: **v1, Linux only, IPv4 only.** Tailbridge runs next to an existing `tailscaled` and manages IPv4 aliases, NAT rules and per-device advertised routes. It is not a VPN.

## What it does

- **LAN → tailnet (feature A).** Gives a remote Tailscale device a local LAN IP alias, so machines on that LAN can reach it at an ordinary local address.

  ```sh
  tailbridge lan add device-b --ip 192.168.1.100 --interface eth0
  # any LAN host:  ssh 192.168.1.100   → device-b on the tailnet
  ```

- **Tailnet → LAN (feature B).** Gives a LAN-only device a virtual IPv4 address that tailnet clients reach through this gateway. The device's real LAN subnet is not exposed to the tailnet.

  ```sh
  tailbridge tailnet add printer --target 192.168.1.80 --ip 10.250.0.10 --interface eth0
  # any tailnet client (after route approval):  10.250.0.10:9100 → printer
  ```

Both directions support multiple mappings per gateway, source and port restrictions, `--dry-run`, continuous reconciliation, and a clean `tailbridge down`.

## Quick start

On Arch Linux, install [`tailbridge-git`](https://aur.archlinux.org/packages/tailbridge-git) from the AUR (for example `paru -S tailbridge-git`). Otherwise:

```sh
make build && sudo install bin/tailbridge /usr/local/bin/
sudo install -m644 contrib/systemd/tailbridge.service /etc/systemd/system/
sudo systemctl enable --now tailbridge

sudo tailbridge diagnose          # check prerequisites
sudo tailbridge lan add device-b --ip 192.168.1.100 --interface eth0
sudo tailbridge tailnet add printer --target 192.168.1.80 --ip 10.250.0.10 --interface eth0
sudo tailbridge status
```

For the full CLI, tailnet policy (ACL/autoApprovers) examples, troubleshooting and the decisions taken in v1, see **[docs/USAGE.md](docs/USAGE.md)**. The original design is in [docs/SPEC.md](docs/SPEC.md).

## Important distinctions

- **Identity.** LAN clients using an A alias reach the peer *as this gateway*. Tailnet ACLs see the gateway's Tailscale identity, not the individual LAN client. That is why aliases are restricted to the LAN interface's own subnets by default.
- **Routed addresses, not Tailscale IPs.** B's virtual IPs are ordinary private addresses advertised as `/32` subnet routes. They are not Tailscale-assigned `100.x.y.z` node addresses. Routes need approval, and Linux clients need `--accept-routes`.
- **Inbound only.** B does not let the LAN device start connections into the tailnet under its virtual address. It is not full bidirectional 1:1 NAT.
- **Owned state only.** Tailbridge owns one nftables table (`ip tailbridge`), the aliases it added and the routes it added. It never flushes or edits rules owned by tailscaled, Docker or the administrator. It preserves every advertised route it did not add, including exit-node routes, and never disables `ip_forward`.

## Development

```sh
make test          # vet + unit tests
make integration   # end-to-end tests in a privileged container with its own network namespace
```

The integration suite builds two sites that share the same `192.168.1.0/24` LAN, a simulated tailnet, and a fake `tailscaled` Local API built on Tailscale's own types. It exercises A and B over TCP, UDP and ICMP, as well as source and port restrictions, collision checks and rollback. It also covers route approval, a peer IP change, a firewall reload, a daemon restart, a reboot, tailscaled being unavailable, and `remove`/`down` leaving foreign rules and routes untouched. It never uses host networking and never talks to the host's `tailscaled`.

## Related Tailscale requests

- [#21869: Expose tailnet devices to the local network through virtual IPs](https://github.com/tailscale/tailscale/issues/21869)
- [#11673: Subnet routing alternative – 1:1 NAT](https://github.com/tailscale/tailscale/issues/11673)
- [#20609: Host-local routes](https://github.com/tailscale/tailscale/issues/20609) (related, not in scope)

> This is an independent project, not an official Tailscale project. Licensed under the [MIT License](LICENSE).
