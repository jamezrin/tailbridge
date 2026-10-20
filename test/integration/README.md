# Integration tests

`make integration` builds `tailbridge` and `fakets`, then runs `run.sh` in a privileged Alpine container.

- The container runs with `--network none`. Every namespace, nftables ruleset and sysctl lives inside the container's own network namespaces, so the host's firewall, routes and `tailscaled` are never touched.
- `fakets` replaces `tailscaled`. It serves `/localapi/v0/status` and `/localapi/v0/prefs` using Tailscale's own `ipnstate`/`ipn` types, reading from a JSON file the test edits to simulate peer IP changes, route approval and so on. It rejects any other request, or any prefs change other than `AdvertiseRoutes`.
- `gw1` emulates a Docker host running tailscaled: `FORWARD` policy DROP plus `ts-forward`/`ts-postrouting` iptables-nft chains, and an admin-owned `inet admin` table. The test asserts these are byte-identical after `down`.
