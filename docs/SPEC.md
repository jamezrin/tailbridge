# Tailbridge: draft technical direction

**Status:** Proposal / discussion draft. A v1 implementing §2A and §2B now exists; see [USAGE.md](USAGE.md) for the decisions it takes on §8.  
**Date:** 2026-10-20  
**Scope:** Linux-first; two directions of device exposure; IPv4 first

This document describes the problem Tailbridge would solve and a plausible first implementation. It is intentionally *not* a final CLI/API specification.

## 1. Motivation

Tailscale works best when devices can run a client, or when a subnet router can expose a whole network. Sometimes neither option is convenient:

- A LAN client cannot install Tailscale, or already uses a VPN that makes simultaneous Tailscale use difficult.
- A printer, camera, PLC, or other legacy/IoT device cannot run Tailscale.
- Two physical LANs use overlapping subnets.
- The user wants to expose **one device**, not manage a whole subnet or modify every client's routes.

Tailbridge would be a small companion to an existing Tailscale installation: map individual devices to convenient IP aliases and keep the underlying forwarding/NAT configuration correct.

Related discussions:
- [tailscale/tailscale#21869](https://github.com/tailscale/tailscale/issues/21869): tailnet device → LAN alias.
- [tailscale/tailscale#11673](https://github.com/tailscale/tailscale/issues/11673): LAN device → tailnet-facing 1:1 mapping. Comments include IPv4-only industrial PLCs and legacy IoT devices that cannot run Tailscale.
- [tailscale/tailscale#20609](https://github.com/tailscale/tailscale/issues/20609): per-host route customization, related but a separate problem.

## 2. Two core features

### A. Expose a tailnet device to the LAN

Example:

```text
LAN client (192.168.1.50)
        │ connects to 192.168.1.100
        ▼
Tailbridge gateway (LAN interface + tailscale0)
        │ DNAT to tailnet peer + SNAT toward tailscale0
        ▼
Remote Tailscale device (100.101.102.103)
```

Conceptual CLI:

```sh
tailbridge lan add device-b --ip 192.168.1.100 --interface eth0
```

Expected behavior:

- The gateway claims a deliberately chosen, available IPv4 alias on its LAN interface.
- LAN traffic to the alias is forwarded to the resolved tailnet peer, retaining destination ports.
- Source NAT allows return traffic without adding routes to the remote peer.
- LAN clients require **no Tailscale installation or routing changes**, provided they can access the gateway's local alias.
- Tailbridge maintains the mapping across gateway reboots, peer IP changes, and Tailscale reconnection.

**Security consequence:** The remote tailnet device normally sees traffic from the *gateway's Tailscale identity*, not the original LAN client's identity. Tailbridge must offer LAN-source restrictions and make this behavior explicit.

### B. Expose an individual LAN device to the tailnet

Example:

```text
Tailscale client
        │ connects to 10.250.0.10
        ▼
Tailbridge gateway advertising 10.250.0.10/32
        │ DNAT + optional SNAT on its LAN interface
        ▼
LAN-only printer (192.168.1.80)
```

Conceptual CLI:

```sh
tailbridge tailnet add printer --target 192.168.1.80 --ip 10.250.0.10 --interface eth0
```

Proposed **first backend:** Tailscale subnet routing with one virtual IPv4 **/32 route per device**:

1. Tailbridge requests advertisement of the /32 using the running Tailscale daemon's Local API.
2. The route is approved by the tailnet administrator (or an existing auto-approval rule).
3. Clients that accept the route send packets to the gateway.
4. On Linux, Tailbridge uses destination NAT to send those packets to the real LAN address, with source NAT where needed for reliable replies.

This provides individually addressable **IPv4** devices and works with legacy devices that cannot run Tailscale or IPv6.

**Important limitation:** `10.250.0.10` is an ordinary routed private address, **not** a Tailscale-assigned device IP in `100.64.0.0/10`. This backend still uses Tailscale subnet routing under the hood. Clients must accept routes when their OS/configuration requires it, and routes must be approved.

The original #11673 also asks for a device to **initiate** connections toward the tailnet while appearing under its virtual address. That is **not included in B's initial inbound-only scope**: the LAN device would need a suitable route through the gateway and additional SNAT/policy handling. We should not market v1 as complete bidirectional 1:1 NAT.

A separate backend using **Tailscale Services / TailVIPs** is worth investigating for native tailnet-managed virtual addresses. It has different approval, service, and protocol semantics, and a TailVIP is not a full independent node identity. In particular, it does not currently cover independently initiated outbound connections from that virtual address.

## 3. Intended scope

**Goals for an initial release**

- A and B for **IPv4 unicast** connections, primarily TCP and UDP.
- Friendly add/list/remove/status/diagnose commands; actual naming TBD.
- Multiple mappings per gateway.
- Idempotent, persistent configuration with automatic reconciliation.
- Target resolution by tailnet node name where feasible.
- Minimal host networking changes, explicit cleanup, and a dry-run / preview mode.
- Optional restrictions on source IPv4 ranges and destination ports.
- Clear visibility into route approval, peer availability, collisions, and firewall state.

**Not goals for the first release**

- Ethernet Layer 2 bridging, broadcasts, multicast service discovery, or transparent LAN extension.
- Guaranteeing compatibility with every PLC/vendor protocol (some embed addresses or require broadcasts).
- Minting arbitrary Tailscale-managed `100.x.y.z` node addresses.
- Creating a replacement VPN, WireGuard stack, or control plane.
- Per-node Tailscale route overrides from #20609.
- Universal Windows/macOS support from day one.
- Automatically changing the local router's DHCP/routing configuration.

## 4. Architecture proposal

A **Go** CLI and privileged background service alongside the *existing* `tailscaled`, initially on Linux.

```text
tailbridge CLI
     │
     ▼
Tailbridge manager / reconciler
     ├── Tailscale adapter (existing tailscaled Local API)
     │     ├── peer status / name resolution
     │     └── per-device advertised /32 routes (feature B)
     ├── IP/interface adapter (Linux netlink)
     ├── NAT/firewall adapter (prefer nftables; assess iptables-nft fallback)
     └── durable Tailbridge-owned mapping configuration
```

Prefer Tailscale's Go Local API client (`tailscale.com/client/local`) over invoking CLI output or importing daemon internals. In particular, `GetPrefs` and `EditPrefs` can be used to inspect and modify advertised routes; `EditPrefs` modifies the **set** of route preferences, not just one element, so Tailbridge must carefully preserve user-managed routes and handle concurrent changes. No unapproved route is presumed to be active.

Avoid importing `wgengine` or private daemon internals in the MVP: building those into Tailbridge does not provide magical control of an already-running daemon. `tsnet` is another option for future work but introduces distinct Tailscale nodes and credentials, which A/B do not need.

The reconciler should converge observed OS/Tailscale state toward Tailbridge's desired mapping configuration after restarts or interface changes. It must never flush or replace Tailscale-managed firewall chains.

## 5. Safety and operational behavior

This is privileged networking software. Safe failure and uninstall behavior are core product requirements:

- Detect address collisions and overlap with existing routes before adding aliases or advertising routes; choose virtual IPs explicitly at first.
- Use dedicated, identifiable Tailbridge-owned firewall tables/chains and rules where possible.
- Apply changes in a deliberate order and roll them back on partial failure.
- Remove **only** Tailbridge-owned addresses, rules, and route advertisements, without touching pre-existing Tailscale or administrator configuration.
- Treat `net.ipv4.ip_forward` changes as global host state: never blindly disable forwarding when another application may need it.
- For A, restrict which LAN clients can use mappings; warn that their connections inherit the gateway's Tailscale identity and effective tailnet permissions.
- For B, retain Tailscale's usual route approval and policy checks; document that a downstream LAN target may see the gateway's LAN IP when SNAT is enabled.
- Expose explicit diagnostics for inactive routes, unreachable destinations, missing privileges, and firewall conflicts.
- Handle stop/uninstall/crash/restart predictably: no stale aliases silently exposing resources and no destructive cleanup.

Public docs must distinguish remote **peer identity** from a translated **virtual address**. Tailbridge cannot manufacture individual ACL identities merely by doing NAT.

## 6. Validation plan

The first useful test matrix is small but real:

| Scenario | Expected result |
| --- | --- |
| A: LAN client without Tailscale connects to virtual LAN IPv4 | TCP/UDP reaches remote tailnet device |
| A: LAN client uses another VPN but retains LAN access | Same result, without adding client routes |
| A: peer disconnects/reconnects or is re-registered | Name-based mapping reconciles or reports a clear error |
| B: tailnet client reaches mapped printer/PLC over IPv4 | Connection succeeds after route approval and acceptance |
| B: two sites use the same physical LAN subnet | Distinct virtual /32 IPs distinguish exposed devices |
| B: route not approved / not accepted on client | Reported as a dependency, not silently assumed to work |
| A/B: overlapping IP, firewall reload, daemon restart | Fail clearly or converge without damaging other rules |
| A/B: remove mapping/uninstall | All Tailbridge-owned state cleaned up; other configuration preserved |

Industrial protocol testing should start with ordinary unicast TCP/UDP protocols; broadcast, multicast, and protocols embedding physical IPs are explicitly out of scope until demonstrated.

## 7. Milestones

1. **Proof of concept (A):** Reproduce known Linux LAN → tailnet behavior using a Go command and managed OS rules.
2. **Usable A:** Add persistence, reconciliation, explicit source restrictions, tests, and clean uninstall.
3. **Proof of concept (B):** Advertise a virtual /32 and NAT it to a single LAN-only device; verify route approval and platform-specific route acceptance.
4. **Usable B:** Multiple mappings, overlap checks, ACL/security documentation, route ownership preservation, and tests.
5. **Explore alternatives:** Tailscale Services/TailVIPs for B; future operating systems and potentially outbound 1:1 NAT if needed.

## 8. Open questions

- Do Layer 3 Tailscale Services provide a better user experience than routed /32 aliases for the common printer/PLC case?
- How should the CLI represent different address types (`LAN alias`, `routed private IP`, `TailVIP`) without suggesting they are interchangeable?
- How should we safely reconcile `AdvertiseRoutes` if another tool or user updates the same preferences concurrently?
- Which Linux firewalls and distributions should the first release support?
- What source-IP restrictions should default to on an A mapping, and should B default to SNAT?
- Is there enough demand for **LAN-initiated** connections under each device's virtual tailnet address to justify a separate advanced mode?

---

*Draft for discussion. Nothing here implies a working implementation or endorsement by Tailscale.*
