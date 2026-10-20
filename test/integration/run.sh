#!/usr/bin/env bash
# End-to-end tests for Tailbridge. Runs inside a privileged container with its
# own network namespace (never host networking); see test/integration/README.md.
#
# Topology (all namespaces live inside the container):
#
#   site 1: lan1 (192.168.1.50), prn1 (192.168.1.80) --br1-- gw1 eth0 (192.168.1.1)
#   site 2:                      prn2 (192.168.1.80) --br2-- gw2 eth0 (192.168.1.1)
#   tailnet: ns "tail" holds peer device-b (100.101.102.103) and client
#            laptop (100.90.0.5); gw1/gw2 "tailscale0" (100.64.0.1/.2) are veths into it.
#
# tailscaled is replaced by fakets (same Local API types), one per gateway.
set -euo pipefail

TB=/tb/tailbridge
FAKETS=/tb/fakets
W=/tmp/it
PASS=0
FAIL=0
PIDS=()

cleanup() { for p in "${PIDS[@]}"; do kill "$p" 2>/dev/null || true; done; }
trap cleanup EXIT

ok() { PASS=$((PASS + 1)); printf '  \e[32mok\e[0m   %s\n' "$*"; }
bad() { FAIL=$((FAIL + 1)); printf '  \e[31mFAIL\e[0m %s\n' "$*"; }
section() { printf '\n\e[1m== %s\e[0m\n' "$*"; }

expect_eq() { # desc got want
	if [[ "$2" == "$3" ]]; then ok "$1"; else bad "$1: got [$2], want [$3]"; fi
}
expect_contains() { # desc haystack needle
	if [[ "$2" == *"$3"* ]]; then ok "$1"; else bad "$1: [$2] does not contain [$3]"; fi
}
expect_success() { # desc cmd...
	local desc=$1; shift
	local out
	if out=$("$@" 2>&1); then ok "$desc"; else bad "$desc: exit $?: $out"; fi
}
expect_failure() { # desc needle cmd...
	local desc=$1 needle=$2; shift 2
	local out
	if out=$("$@" 2>&1); then
		bad "$desc: unexpectedly succeeded: $out"
	elif [[ "$out" == *"$needle"* ]]; then
		ok "$desc"
	else
		bad "$desc: failed without [$needle]: $out"
	fi
}
# eventually DESC TIMEOUT CMD... retries until CMD succeeds.
eventually() {
	local desc=$1 t=$2; shift 2
	local end=$((SECONDS + t))
	while ((SECONDS < end)); do
		if "$@" >/dev/null 2>&1; then ok "$desc"; return; fi
		sleep 0.5
	done
	bad "$desc (timed out after ${t}s)"
}

tb() { # gateway args...
	local gw=$1; shift
	ip netns exec "$gw" "$TB" --config "$W/$gw/config.json" --state-dir "$W/$gw/state" --socket "$W/$gw/ts.sock" "$@"
}
tb1() { tb gw1 "$@"; }
tb2() { tb gw2 "$@"; }

# Client helpers print whatever the server answered (empty on failure).
tcp() { # ns dst port [bind]
	local opts=""
	[[ -n "${4:-}" ]] && opts=",bind=$4"
	ip netns exec "$1" timeout 4 socat -T2 - "TCP4:$2:$3,connect-timeout=2$opts" </dev/null 2>/dev/null || true
}
udp() { # ns dst port [bind]
	local opts=""
	[[ -n "${4:-}" ]] && opts=",bind=$4"
	echo ping | ip netns exec "$1" timeout 3 socat -T2 - "UDP4:$2:$3$opts" 2>/dev/null | head -n1 || true
}
serve() { # ns ip tcpport udpport label
	ip netns exec "$1" socat "TCP4-LISTEN:$3,bind=$2,fork,reuseaddr" SYSTEM:"echo $5 seen-from \$SOCAT_PEERADDR" &
	PIDS+=($!)
	ip netns exec "$1" socat "UDP4-RECVFROM:$4,bind=$2,fork" SYSTEM:"echo $5-udp seen-from \$SOCAT_PEERADDR" &
	PIDS+=($!)
}

fake() { # gw jq-filter: edit a fake tailscaled's state
	local f=$W/$1/fakets.json
	jq "$2" "$f" >"$f.new" && mv "$f.new" "$f"
}
routes() { jq -c '.advertiseRoutes' "$W/$1/fakets.json"; }

start_fakets() { # gw
	"$FAKETS" -socket "$W/$1/ts.sock" -state "$W/$1/fakets.json" 2>>"$W/$1/fakets.log" &
	eval "FAKETS_$1=$!"
	PIDS+=($!)
	for _ in $(seq 50); do [[ -S "$W/$1/ts.sock" ]] && return; sleep 0.1; done
	echo "fakets $1 did not start" >&2
	exit 1
}

snapshot_foreign_rules() { # gw: everything not owned by tailbridge, without counters
	{
		ip netns exec "$1" iptables-nft-save | grep -v '^#' | sed -E 's/\[[0-9]+:[0-9]+\]//'
		ip netns exec "$1" nft list table inet admin | sed -E 's/counter packets [0-9]+ bytes [0-9]+/counter/'
	}
}

###########################################################################
section "topology"

mkdir -p "$W"
for ns in gw1 gw2 lan1 prn1 prn2 tail; do ip netns add "$ns"; ip -n "$ns" link set lo up; done

for site in 1 2; do
	ip link add "br$site" type bridge
	ip link set "br$site" up
done
attach() { # site ns addr
	ip link add "v-$2" type veth peer name eth0 netns "$2"
	ip link set "v-$2" master "br$1" up
	ip -n "$2" addr add "$3/24" dev eth0
	ip -n "$2" link set eth0 up
}
attach 1 gw1 192.168.1.1
attach 1 lan1 192.168.1.50
attach 1 prn1 192.168.1.80
attach 2 gw2 192.168.1.1
attach 2 prn2 192.168.1.80
# Printers have a default route to a router that is not the gateway, so
# replies to tailnet addresses only come back through the gateway with SNAT.
ip -n prn1 route add default via 192.168.1.254
ip -n prn2 route add default via 192.168.1.254

# Tailnet side.
ip -n tail addr add 100.101.102.103/32 dev lo
ip -n tail addr add 100.90.0.5/32 dev lo
for i in 1 2; do
	ip link add tailscale0 netns "gw$i" type veth peer name "t-gw$i" netns tail
	ip -n "gw$i" addr add "100.64.0.$i/32" dev tailscale0
	ip -n "gw$i" link set tailscale0 up
	ip -n tail link set "t-gw$i" up
	ip -n tail route add "100.64.0.$i/32" dev "t-gw$i"
	# Like tailscaled: tailnet routes live in table 52.
	ip -n "gw$i" route add 100.64.0.0/10 dev tailscale0 table 52
	ip -n "gw$i" rule add pref 5270 table 52
	# Gateways have a default route to the LAN router (absent from the lab), so
	# untranslated traffic would leak out of it unless Tailbridge drops it.
	ip -n "gw$i" route add default via 192.168.1.254
	ip netns exec "gw$i" sysctl -qw net.ipv4.ip_forward=0
done
# The laptop "accepted" the routes advertised by each gateway.
ip -n tail route add 10.250.0.10/32 via 100.64.0.1 dev t-gw1 onlink
ip -n tail route add 10.250.0.12/32 via 100.64.0.1 dev t-gw1 onlink
ip -n tail route add 10.250.0.11/32 via 100.64.0.2 dev t-gw2 onlink
ip -n tail route add 10.250.0.20/32 via 100.64.0.2 dev t-gw2 onlink

serve tail 100.101.102.103 8080 5353 peer-b
serve prn1 192.168.1.80 9100 9101 printer-site1
serve prn2 192.168.1.80 9100 9101 printer-site2
# A service on the gateway itself, which must never be exposed via aliases.
ip netns exec gw1 socat TCP4-LISTEN:9999,fork,reuseaddr SYSTEM:"echo gateway-service" &
PIDS+=($!)

# Fake tailscaled per gateway. Both already advertise an exit node, and
# gw1 advertises a user-managed /32 that Tailbridge must never remove.
for i in 1 2; do
	mkdir -p "$W/gw$i"
	cat >"$W/gw$i/fakets.json" <<EOF
{
  "backendState": "Running",
  "self": {"hostName": "gw$i", "dnsName": "gw$i.example.ts.net.", "ips": ["100.64.0.$i"], "online": true},
  "peers": [
    {"hostName": "device-b", "dnsName": "device-b.example.ts.net.", "ips": ["100.101.102.103", "fd7a:115c:a1e0::1"], "online": true},
    {"hostName": "laptop", "dnsName": "laptop.example.ts.net.", "ips": ["100.90.0.5"], "online": true},
    {"hostName": "router2", "dnsName": "router2.example.ts.net.", "ips": ["100.90.0.9"], "online": true, "primaryRoutes": ["10.99.0.0/16"]}
  ],
  "approve": true,
  "advertiseRoutes": ["0.0.0.0/0", "::/0"]
}
EOF
	start_fakets "gw$i"
done
fake gw1 '.advertiseRoutes += ["192.0.2.7/32"]'
# gw2 already advertises 10.250.0.20/32 itself (user-managed).
fake gw2 '.advertiseRoutes += ["10.250.0.20/32"]'

# gw1 emulates a Docker host running tailscaled with netfilter management:
# FORWARD policy DROP plus tailscaled's ts-forward/ts-postrouting chains,
# and an administrator-owned nft table.
ip netns exec gw1 bash -e <<'EOF'
iptables-nft -P FORWARD DROP
iptables-nft -N ts-forward
iptables-nft -A FORWARD -j ts-forward
iptables-nft -A ts-forward -i tailscale0 -j MARK --set-xmark 0x40000/0xff0000
iptables-nft -A ts-forward -m mark --mark 0x40000/0xff0000 -j ACCEPT
iptables-nft -A ts-forward -o tailscale0 -j ACCEPT
iptables-nft -t nat -N ts-postrouting
iptables-nft -t nat -A POSTROUTING -j ts-postrouting
iptables-nft -t nat -A ts-postrouting -m mark --mark 0x40000/0xff0000 -j MASQUERADE
nft add table inet admin
nft add chain inet admin input '{ type filter hook input priority 0; policy accept; }'
nft add rule inet admin input tcp dport 2222 counter accept
EOF
# gw2 emulates Docker's FORWARD DROP *without* tailscaled's rules.
ip netns exec gw2 bash -e <<'EOF'
iptables-nft -P FORWARD DROP
nft add table inet admin
nft add chain inet admin input '{ type filter hook input priority 0; policy accept; }'
EOF

BEFORE_GW1=$(snapshot_foreign_rules gw1)
BEFORE_GW2=$(snapshot_foreign_rules gw2)
ok "topology ready"

###########################################################################
section "diagnose before any mapping"

out=$(tb2 diagnose 2>&1 || true)
expect_contains "gw2: diagnose flags FORWARD DROP without Tailscale rules" "$out" "policy drop and no Tailscale ts-forward rules"
out=$(tb1 diagnose 2>&1 || true)
expect_contains "gw1: diagnose sees ts-forward accepting tailnet traffic" "$out" "Tailscale's ts-forward rules accept"
expect_contains "gw1: diagnose reports tailscaled running" "$out" "backend Running"

###########################################################################
section "A: tailnet peer -> LAN alias"

out=$(tb1 lan add device-b --ip 192.168.1.100 --interface eth0 --dry-run --show-rules)
expect_contains "dry-run prints the plan" "$out" "add address 192.168.1.100/32 to eth0"
expect_contains "dry-run prints the nft table" "$out" 'dnat to 100.101.102.103'
expect_eq "dry-run did not add the alias" "$(ip -n gw1 -br addr show eth0 | grep -c 192.168.1.100 || true)" "0"
expect_eq "dry-run did not write config" "$(test -e $W/gw1/config.json && echo yes || echo no)" "no"

expect_success "lan add device-b" tb1 lan add device-b --ip 192.168.1.100 --interface eth0
expect_eq "ip_forward enabled" "$(ip netns exec gw1 cat /proc/sys/net/ipv4/ip_forward)" "1"
expect_contains "alias claimed as /32" "$(ip -n gw1 -br addr show eth0)" "192.168.1.100/32"
expect_eq "TCP from LAN reaches peer, which sees the gateway's tailnet IP" "$(tcp lan1 192.168.1.100 8080)" "peer-b seen-from 100.64.0.1"
expect_eq "UDP from LAN reaches peer" "$(udp lan1 192.168.1.100 5353)" "peer-b-udp seen-from 100.64.0.1"
expect_success "ICMP to the alias is forwarded to the peer" ip netns exec lan1 ping -c1 -W2 192.168.1.100
expect_eq "gateway's own service is NOT reachable through the alias" "$(tcp lan1 192.168.1.100 9999)" ""
expect_eq "control: gateway's own service is reachable on its real IP" "$(tcp lan1 192.168.1.1 9999)" "gateway-service"
st=$(tb1 status --json)
expect_eq "status: active" "$(jq -r '.mappings[0].state' <<<"$st")" "active"
expect_eq "status: resolved peer" "$(jq -r '.mappings[0].peerIP' <<<"$st")" "100.101.102.103"
expect_eq "status: default allowed sources are the LAN subnet" "$(jq -c '.mappings[0].allowSources' <<<"$st")" '["192.168.1.0/24"]'
expect_eq "status: no drift" "$(jq -r '.plan | del(.ruleset) | length' <<<"$st")" "0"

expect_success "lan add with port restriction" tb1 lan add device-b.example.ts.net --name b-web --ip 192.168.1.101 --interface eth0 --port tcp:8080
expect_eq "allowed port is translated" "$(tcp lan1 192.168.1.101 8080)" "peer-b seen-from 100.64.0.1"
expect_eq "other protocols/ports are dropped" "$(udp lan1 192.168.1.101 5353)" ""
expect_eq "gateway service not reachable through restricted alias" "$(tcp lan1 192.168.1.101 9999)" ""

expect_success "lan add with source restriction" tb1 lan add 100.101.102.103 --name b-restricted --ip 192.168.1.102 --interface eth0 --allow-src 192.168.1.60/32
expect_eq "disallowed LAN source is dropped" "$(tcp lan1 192.168.1.102 8080)" ""

section "A: pre-flight and rollback"
cfg_before=$(cat "$W/gw1/config.json")
expect_failure "alias used by another LAN host (ARP probe)" "answers ARP" tb1 lan add device-b --name dup --ip 192.168.1.50 --interface eth0
expect_failure "alias already on the gateway" "already assigned to eth0" tb1 lan add device-b --name dup --ip 192.168.1.1 --interface eth0
expect_failure "alias outside the LAN subnet" "outside the connected subnets" tb1 lan add device-b --name dup --ip 10.9.9.9 --interface eth0
expect_failure "unknown peer is rolled back" "no tailnet peer matches" tb1 lan add nosuch --ip 192.168.1.103 --interface eth0
expect_failure "the gateway itself is rejected as a peer" "this gateway itself" tb1 lan add gw1 --ip 192.168.1.103 --interface eth0
expect_failure "missing interface" "does not exist" tb1 lan add device-b --name dup --ip 192.168.1.103 --interface eth9 --force
expect_eq "config unchanged after failed adds" "$(cat "$W/gw1/config.json")" "$cfg_before"
expect_eq "no stray alias after failed adds" "$(ip -n gw1 -br addr show eth0 | grep -c 192.168.1.103 || true)" "0"

###########################################################################
section "B: LAN device -> tailnet /32"

expect_success "tailnet add printer (gw1)" tb1 tailnet add printer --target 192.168.1.80 --ip 10.250.0.10 --interface eth0
expect_eq "route advertised, foreign routes preserved" "$(routes gw1)" '["0.0.0.0/0","::/0","192.0.2.7/32","10.250.0.10/32"]'
expect_eq "TCP from tailnet reaches printer via SNAT" "$(tcp tail 10.250.0.10 9100 100.90.0.5)" "printer-site1 seen-from 192.168.1.1"
expect_eq "UDP from tailnet reaches printer" "$(udp tail 10.250.0.10 9101 100.90.0.5)" "printer-site1-udp seen-from 192.168.1.1"
st=$(tb1 status --json)
expect_eq "status: printer active and approved" "$(jq -r '.mappings[] | select(.name=="printer") | .state + "/" + .route' <<<"$st")" "active/approved"

expect_success "tailnet add with port restriction" tb1 tailnet add printer-raw --target 192.168.1.80 --ip 10.250.0.12 --interface eth0 --port tcp:9100
expect_eq "allowed port translated" "$(tcp tail 10.250.0.12 9100 100.90.0.5)" "printer-site1 seen-from 192.168.1.1"
expect_eq "other ports dropped" "$(udp tail 10.250.0.12 9101 100.90.0.5)" ""
drops=$(ip netns exec gw1 nft -j list table ip tailbridge | jq '[.nftables[] | .rule? | select(.comment == "tailnet:printer-raw:untranslated") | .expr[] | .counter? | select(.) | .packets] | add')
if ((drops > 0)); then ok "untranslated tailnet traffic dropped instead of routed"; else bad "untranslated drop counter is $drops"; fi

section "B: two sites with the same LAN subnet"
ip netns exec gw2 iptables-nft -P FORWARD ACCEPT
BEFORE_GW2=$(snapshot_foreign_rules gw2)
expect_success "tailnet add printer (gw2)" tb2 tailnet add printer --target 192.168.1.80 --ip 10.250.0.11 --interface eth0
expect_eq "site 1 printer via 10.250.0.10" "$(tcp tail 10.250.0.10 9100 100.90.0.5)" "printer-site1 seen-from 192.168.1.1"
expect_eq "site 2 printer via 10.250.0.11" "$(tcp tail 10.250.0.11 9100 100.90.0.5)" "printer-site2 seen-from 192.168.1.1"

section "B: approval, collisions and ownership"
fake gw2 '.approve = false'
st=$(tb2 status --json)
expect_eq "unapproved route reported as pending" "$(jq -r '.mappings[0].state + "/" + .mappings[0].route' <<<"$st")" "pending-approval/pending-approval"
fake gw2 '.approve = true'
expect_eq "approved again" "$(tb2 status --json | jq -r '.mappings[0].state')" "active"

expect_failure "vip inside the LAN subnet" "overlaps existing routes" tb1 tailnet add bad --target 192.168.1.80 --ip 192.168.1.200 --interface eth0
expect_failure "vip overlapping another node's routes" "advertised by other tailnet nodes" tb1 tailnet add bad --target 192.168.1.80 --ip 10.99.0.5 --interface eth0
expect_failure "target not reachable via interface" "routed via tailscale0" tb1 tailnet add bad --target 100.101.102.103 --ip 10.250.0.13 --interface eth0
expect_eq "no route left behind by failed adds" "$(routes gw1)" '["0.0.0.0/0","::/0","192.0.2.7/32","10.250.0.10/32","10.250.0.12/32"]'

expect_success "tailnet add on an already user-advertised /32" tb2 tailnet add printer-alt --target 192.168.1.80 --ip 10.250.0.20 --interface eth0
expect_eq "user-managed route reaches printer" "$(tcp tail 10.250.0.20 9100 100.90.0.5)" "printer-site2 seen-from 192.168.1.1"
expect_success "remove printer-alt" tb2 remove printer-alt
expect_eq "user-managed route survives removal (not owned)" "$(routes gw2)" '["0.0.0.0/0","::/0","10.250.0.20/32","10.250.0.11/32"]'

###########################################################################
section "reconciliation (daemon)"

ip netns exec gw1 "$TB" --config "$W/gw1/config.json" --state-dir "$W/gw1/state" --socket "$W/gw1/ts.sock" daemon --interval 2s 2>"$W/gw1/daemon.log" &
DAEMON=$!
PIDS+=($DAEMON)
sleep 1

# Peer re-registered with a new IP.
ip -n tail addr add 100.101.102.104/32 dev lo
serve tail 100.101.102.104 8080 5353 peer-b2
fake gw1 '(.peers[] | select(.hostName=="device-b") | .ips) = ["100.101.102.104"]'
check_peer2() { [[ "$(tcp lan1 192.168.1.100 8080)" == "peer-b2 seen-from 100.64.0.1" ]]; }
eventually "peer IP change is followed" 10 check_peer2

# Admin removes the alias by hand / interface flap.
ip -n gw1 addr del 192.168.1.100/32 dev eth0
has_alias() { ip -n gw1 -br addr show eth0 | grep -q 192.168.1.100/32; }
eventually "deleted alias is restored" 10 has_alias

# Firewall reload that wipes our table.
ip netns exec gw1 nft delete table ip tailbridge
has_table() { ip netns exec gw1 nft list table ip tailbridge >/dev/null; }
eventually "deleted nft table is restored" 10 has_table
eventually "traffic works again after firewall reload" 10 check_peer2

# Someone runs `tailscale set --advertise-routes=` and drops our route.
fake gw1 '.advertiseRoutes = ["0.0.0.0/0","::/0","192.0.2.7/32"]'
has_route() { routes gw1 | grep -q 10.250.0.10/32; }
eventually "withdrawn route is re-advertised" 10 has_route

kill "$DAEMON"; wait "$DAEMON" 2>/dev/null || true
expect_eq "stopping the daemon leaves state in place" "$(tcp lan1 192.168.1.100 8080)" "peer-b2 seen-from 100.64.0.1"

section "restart and reboot"
writes=$(jq .writes "$W/gw1/fakets.json")
ip netns exec gw1 "$TB" --config "$W/gw1/config.json" --state-dir "$W/gw1/state" --socket "$W/gw1/ts.sock" daemon --interval 1s 2>"$W/gw1/daemon2.log" &
DAEMON=$!
PIDS+=($DAEMON)
sleep 3
kill "$DAEMON"; wait "$DAEMON" 2>/dev/null || true
expect_eq "daemon restart makes no changes" "$(grep -c 'msg=reconciled' "$W/gw1/daemon2.log" || true)" "0"
expect_eq "daemon restart does not rewrite prefs" "$(jq .writes "$W/gw1/fakets.json")" "$writes"

# Simulate a reboot: kernel state is gone, config and state files remain.
ip -n gw1 addr del 192.168.1.100/32 dev eth0
ip -n gw1 addr del 192.168.1.101/32 dev eth0
ip -n gw1 addr del 192.168.1.102/32 dev eth0
ip netns exec gw1 nft delete table ip tailbridge
expect_success "apply after reboot" tb1 apply
expect_eq "A works after reboot" "$(tcp lan1 192.168.1.100 8080)" "peer-b2 seen-from 100.64.0.1"
expect_eq "B works after reboot" "$(tcp tail 10.250.0.10 9100 100.90.0.5)" "printer-site1 seen-from 192.168.1.1"

section "tailscaled unavailable"
kill "$FAKETS_gw1"; wait "$FAKETS_gw1" 2>/dev/null || true
rm -f "$W/gw1/ts.sock"
ip netns exec gw1 nft delete table ip tailbridge
out=$(tb1 apply 2>&1)
expect_contains "apply warns that tailscaled is unavailable" "$out" "tailscaled unavailable"
expect_eq "A keeps working from the cached peer address" "$(tcp lan1 192.168.1.100 8080)" "peer-b2 seen-from 100.64.0.1"
start_fakets gw1

###########################################################################
section "remove and down"

expect_success "remove printer" tb1 remove printer
expect_eq "only the owned route was withdrawn" "$(routes gw1)" '["0.0.0.0/0","::/0","192.0.2.7/32","10.250.0.12/32"]'
expect_eq "removed vip no longer reachable" "$(tcp tail 10.250.0.10 9100 100.90.0.5)" ""
expect_success "remove b-web" tb1 remove b-web
expect_eq "removed alias released" "$(ip -n gw1 -br addr show eth0 | grep -c 192.168.1.101 || true)" "0"

out=$(tb1 down --dry-run)
expect_contains "down --dry-run lists removals" "$out" "remove address 192.168.1.100/32 from eth0"
expect_success "down --dry-run changed nothing" has_alias

for gw in gw1 gw2; do
	expect_success "$gw: down" tb "$gw" down
	expect_eq "$gw: no tailbridge table" "$(ip netns exec $gw nft list tables | grep -c tailbridge || true)" "0"
	expect_eq "$gw: no aliases left" "$(ip -4 -n $gw -br addr show eth0 | awk '{print $3,$4,$5}' | xargs)" "192.168.1.1/24"
	expect_eq "$gw: ip_forward left enabled" "$(ip netns exec $gw cat /proc/sys/net/ipv4/ip_forward)" "1"
done
expect_eq "gw1: foreign routes preserved" "$(routes gw1)" '["0.0.0.0/0","::/0","192.0.2.7/32"]'
expect_eq "gw2: foreign routes preserved" "$(routes gw2)" '["0.0.0.0/0","::/0","10.250.0.20/32"]'
expect_eq "gw1: iptables and admin nft rules untouched" "$(snapshot_foreign_rules gw1)" "$BEFORE_GW1"
expect_eq "gw2: iptables and admin nft rules untouched" "$(snapshot_foreign_rules gw2)" "$BEFORE_GW2"
expect_success "config kept after down" grep -q device-b "$W/gw1/config.json"
expect_success "apply restores after down" tb1 apply
expect_eq "A works again" "$(tcp lan1 192.168.1.100 8080)" "peer-b2 seen-from 100.64.0.1"
expect_success "final down" tb1 down

if grep -h "unexpected" "$W"/gw*/fakets.log; then bad "fake tailscaled saw unexpected requests"; else ok "fake tailscaled saw only expected Local API calls"; fi

printf '\n%d passed, %d failed\n' "$PASS" "$FAIL"
((FAIL == 0))
