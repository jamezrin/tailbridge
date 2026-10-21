# Tailbridge v1 usage

## Requirements

- Linux with nftables (the `nft` binary; nftables ≥ 0.9.7 and kernel ≥ 5.10 so the table comment used for drift detection is kept) and root privileges.
- A running `tailscaled` using a kernel TUN interface (`tailscale0`). Userspace-networking mode is not supported.
- IPv4.

## Concepts

| Term | Meaning |
| --- | --- |
| **lan mapping** (feature A) | A **LAN alias**: an IPv4 address Tailbridge claims as a `/32` on a LAN interface. Traffic to it is DNATed to a tailnet peer and masqueraded to the gateway's Tailscale IP. |
| **tailnet mapping** (feature B) | A **routed virtual IP**: a private IPv4 `/32` that this gateway advertises as a Tailscale subnet route. Traffic arriving from `tailscale0` is DNATed to the device's real LAN address and, by default, masqueraded to the gateway's LAN address. |

Neither kind is a Tailscale node IP or a TailVIP. Neither gives a device its own tailnet identity.

## Commands

All commands accept `--config` (default `/etc/tailbridge/config.json`), `--state-dir` (default `/var/lib/tailbridge`), `--socket` (default `/var/run/tailscale/tailscaled.sock`) and `--json`.

### `tailbridge lan add PEER --ip ALIAS --interface IFACE`

Exposes the tailnet peer `PEER` on the LAN at `ALIAS`.

`PEER` can be a Tailscale IPv4 address or a full MagicDNS name. Nodes in this gateway's own tailnet can also be named by their short MagicDNS name or hostname. Nodes shared in from other tailnets must use their full name or IP, because their hostnames are chosen by someone else.

The first resolution **pins the node** (its stable node ID):

- If the pinned node's IP changes, the mapping follows it.
- If the name later resolves to a *different* node, or to nothing, the mapping fails closed and its alias is released. Remove and re-add the mapping to trust a new node.
- A mapping that names its peer by IP fails closed if that IP moves to another peer or disappears.
- The last known address is used only while tailscaled itself is unreachable.

| Flag | Default | |
| --- | --- | --- |
| `--name` | derived from `PEER` | Mapping name. |
| `--allow-src CIDR` | the interface's connected subnets | Repeatable. Use `any` for `0.0.0.0/0`. |
| `--port tcp:22`, `--port udp:5000-5010` | all IPv4 traffic, including ICMP | Repeatable. |
| `--force` | | Skip the soft pre-flight checks: alias outside the interface subnet, or another host answering ARP for it. |
| `--dry-run [--show-rules]` | | Print the plan (and the full nftables table) without changing anything. |

Traffic to the alias that is not translated, because its source or port is not allowed, is **dropped**. It never reaches the gateway's own services.

### `tailbridge tailnet add NAME --target LAN-IP --ip VIRTUAL-IP --interface IFACE`

Exposes the LAN device `LAN-IP` to the tailnet at `VIRTUAL-IP`.

| Flag | Default | |
| --- | --- | --- |
| `--allow-src CIDR` | any tailnet source (Tailscale policy still applies) | Repeatable. |
| `--port` | all traffic | Same format as for `lan add`. |
| `--no-snat` | SNAT on | Don't masquerade on the LAN side. The device then sees real tailnet source IPs, but it must route `100.64.0.0/10` (and any other client ranges) back through this gateway. |
| `--force`, `--dry-run`, `--show-rules` | | As for `lan add`. |

The virtual IP must not overlap any local address or route, the tailnet's `100.64.0.0/10`, or any route another tailnet node serves. The target must route through `IFACE`.

Traffic to the virtual IP that is not translated is dropped. It is never routed elsewhere, for example out of the default route.

### Other commands

| Command | |
| --- | --- |
| `remove NAME` (`rm`; also `lan remove`, `tailnet remove`) | Removes the mapping and only the state Tailbridge owns for it. |
| `list` | Shows the configured mappings (no root needed). |
| `status` | Shows live per-mapping state and any drift between host and config:<br>• `active`<br>• `pending-approval`: the route is advertised but not approved for this node<br>• `error`: with the reason |
| `apply [--dry-run]` | Reconciles once. |
| `daemon [--interval 30s]` | Reconciles continuously: at startup, periodically, on netlink link/address changes and on `SIGHUP`. Stopping it leaves state in place. |
| `down [--dry-run]` | Removes every Tailbridge-owned alias, its nftables table and the advertised routes it added. Keeps the configuration; `apply` restores it. Stop the daemon first. |
| `diagnose` | Checks privileges, nft, `ip_forward`, `tailscaled` (backend state, netfilter mode, accept-routes), the interfaces, forward chains with policy drop that lack Tailscale's `ts-forward` accepts (for example Docker), legacy iptables, drift, and every mapping. |

`add` is **transactional**. If the new mapping cannot become active (peer not found, address or route collision, missing interface, …), the previous configuration is restored and reconciled again.

## Tailnet policy

Example policy for a gateway tagged `tag:tailbridge`:

```jsonc
{
  "tagOwners": { "tag:tailbridge": ["autogroup:admin"] },

  // Feature B: approve the gateway's virtual /32s automatically.
  "autoApprovers": {
    "routes": { "10.250.0.0/24": ["tag:tailbridge"] }
  },

  "grants": [
    // Feature B: who may reach the exposed LAN devices.
    { "src": ["group:office"], "dst": ["10.250.0.10/32"], "ip": ["tcp:9100"] },

    // Feature A: LAN clients appear as the gateway, so the *gateway* needs
    // access to the peer. Keep this as narrow as possible.
    { "src": ["tag:tailbridge"], "dst": ["tag:nas"], "ip": ["tcp:445"] }
  ]
}
```

On Linux clients, run `tailscale set --accept-routes` to use feature B mappings. Other platforms accept routes by default.

## How it works

Tailbridge keeps two files:

- **Config** (`/etc/tailbridge/config.json`): the desired mappings.
- **State** (`/var/lib/tailbridge/state.json`): which aliases and advertised routes Tailbridge itself created, plus the last resolved peer IPs.

The CLI and daemon share a file lock.

Each reconcile does the following:

1. Reads `tailscaled` status and prefs over the Local API, plus interfaces, addresses and routes over netlink, plus Tailbridge's nftables table hash.
2. Checks every mapping. Mappings that fail are not applied, and stale state for them is removed.
3. Applies changes in a safe order:
   1. Withdraws routes that are no longer wanted.
   2. Removes stale aliases.
   3. Atomically replaces the `ip tailbridge` table.
   4. Adds new aliases.
   5. Advertises new routes.

   Ownership is written to the state file *before* each resource is created. If the firewall step fails, no alias or route is added.

Advertised routes are changed with a read-modify-write of `AdvertiseRoutes` (`GetPrefs` + `EditPrefs`). Routes that Tailbridge did not add are never removed, including exit-node routes and routes that were already advertised before a mapping asked for them. The write is verified afterwards. If another tool overwrites the list, the next reconcile re-adds Tailbridge's routes. If you run `tailscale up`, remember that it resets unspecified flags; prefer `tailscale set`.

The `ip tailbridge` table contains:

- **`prerouting`** (nat, priority −110): DNAT, matched on the input interface, the alias or virtual IP, the allowed sources and the ports.
- **`postrouting`** (nat, priority 90): masquerades flows Tailbridge DNATed (`ct status dnat ct original ip daddr …`).
- **`input`** (filter, priority −10): drops untranslated traffic to aliases.
- **`forward`** (filter, priority −10): drops untranslated traffic to virtual IPs.

Inspect it with `nft list table ip tailbridge`, or preview it with `tailbridge apply --dry-run`.

## Decisions taken in v1 (spec §8)

- **Address types** stay separate in the CLI: `lan` for LAN aliases, `tailnet` for routed private IPs. TailVIP is not implemented.
- **Concurrent `AdvertiseRoutes` changes** are handled with ownership tracking, read-modify-write immediately before each write, post-write verification and continuous reconciliation. The Local API has no compare-and-swap, so a race in the instant between read and write remains possible. It heals on the next reconcile.
- **Firewall support:** nftables only, via the `nft` binary. It coexists with iptables-nft rules (tailscaled, Docker). Legacy iptables rules are invisible to it, and `diagnose` warns about them.
- **Default source restriction for A:** the LAN interface's connected subnets. **B defaults to SNAT** so devices without a route back to the tailnet still work. Use `--no-snat` to preserve source IPs.
- **LAN-initiated 1:1 NAT** is not implemented.

## Troubleshooting

| Symptom | Check |
| --- | --- |
| `pending-approval` | Approve the `/32` in the admin console (Machines → the gateway → Edit route settings) or add an `autoApprovers` rule. |
| B works from macOS/iOS but not from a Linux client | `tailscale set --accept-routes` on the client. |
| Nothing is forwarded on a Docker host | `diagnose` reports `FORWARD` policy drop. Let tailscaled manage netfilter (the default), or allow forwarding between the LAN and `tailscale0`. |
| LAN client gets no answer from the alias | Is the client in `--allow-src` (default: the interface subnet)? Do the ports match? Does the tailnet policy allow *the gateway* to reach the peer? |
| Device behind B never answers | With `--no-snat`, the device needs a route back via the gateway. Otherwise, check that the target really routes via `--interface`. |

## Manual test: LAN client on another VPN

This test is not automated. It covers spec §6, "LAN client uses another VPN but retains LAN access".

1. Connect a LAN client to a full-tunnel VPN that still allows local LAN access (most clients call this "allow LAN access").
2. From that client, connect to the A alias, for example `ssh 192.168.1.100`.
3. **Expected:** it works without client route changes, because the alias is an on-link LAN address.

If the VPN blocks LAN access entirely, Tailbridge cannot help.

## Uninstall

Removing the package stops and disables the service, then runs `tailbridge down`, so no Tailbridge-owned aliases, rules or routes are left behind:

- **Debian, Ubuntu:** `sudo apt remove tailbridge`
- **Fedora, RHEL:** `sudo dnf remove tailbridge`
- **Arch:** `sudo pacman -R tailbridge-git`. Run `sudo systemctl disable --now tailbridge && sudo tailbridge down` first.
- **install.sh:** `sudo install.sh --uninstall`

The configuration (`/etc/tailbridge`) and state (`/var/lib/tailbridge`) are kept. Delete them to remove everything.
