# Tailbridge

**Tailbridge** is a proposed, open-source companion to [Tailscale](https://tailscale.com/) for making individual devices reachable across a LAN and a tailnet without installing Tailscale on every device or configuring routes on every client.

The project is **in the design stage**. There is no working Tailbridge release yet.

## What it aims to do

- **LAN → tailnet:** Give a remote Tailscale device a local LAN IP alias, so machines on that LAN can access it using an ordinary local address.
- **Tailnet → LAN:** Give a LAN-only device a virtual IPv4 address reachable through a Tailscale gateway, without exposing the device's real LAN subnet to the tailnet.

The initial direction is a **Linux-first** Go tool that integrates with an existing `tailscaled` installation. It would manage routing/NAT and persistent mappings rather than implementing a new VPN.

See **[Draft specification](docs/SPEC.md)** for scope, examples, proposed architecture, constraints, and open questions.

## Related Tailscale requests

- [#21869: Expose tailnet devices to the local network through virtual IPs](https://github.com/tailscale/tailscale/issues/21869)
- [#11673: Subnet routing alternative – 1:1 NAT](https://github.com/tailscale/tailscale/issues/11673)
- [#20609: Host-local routes](https://github.com/tailscale/tailscale/issues/20609) (related, but not currently in scope)

## Important distinctions

Tailbridge's initial tailnet-facing aliases would use **Tailscale subnet routing of individual IPv4 /32 addresses**, not additional Tailscale-assigned node IPs. Route approval and client route acceptance would still apply. Tailscale Services/TailVIPs are worth evaluating as an alternative backend, but are not assumed in the MVP.

Because address translation can make clients appear under the gateway's identity or IP, security boundaries and access restrictions are part of the design, not optional afterthoughts.

> This is an independent proposal, not an official Tailscale project.
