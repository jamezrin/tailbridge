#!/usr/bin/env bash
# Install, run and remove the release artifacts in clean distro containers.
# Usage: test/packaging/smoke.sh DIST_DIR   (amd64 artifacts only)
set -euo pipefail
DIST=$(cd "${1:-dist}" && pwd)
ROOT=$(cd "$(dirname "$0")/../.." && pwd)
deb=$(cd "$DIST" && ls tailbridge_*_amd64.deb)
rpm=$(cd "$DIST" && ls tailbridge-*.x86_64.rpm)
tgz=$(cd "$DIST" && ls tailbridge_*_linux_amd64.tar.gz)
FAIL=0

run() { # name image script
	printf '\n== %s (%s)\n' "$1" "$2"
	if docker run --rm --cap-add NET_ADMIN -v "$DIST:/dist:ro" -v "$ROOT/install.sh:/install.sh:ro" "$2" sh -euc "$3"; then
		echo "-- ok: $1"
	else
		echo "-- FAIL: $1"
		FAIL=1
	fi
}

# Common assertions once tailbridge is installed at $1.
check='
	bin=$1
	$bin --version
	$bin --version | grep -qv " dev$"
	test -f "$2"
	grep -q "^ExecStart=$bin daemon" "$2"
	$bin list
'

run "deb on debian" debian:stable "
	apt-get update -qq >/dev/null
	apt-get install -y -qq /dist/$deb >/dev/null
	set -- /usr/bin/tailbridge /usr/lib/systemd/system/tailbridge.service; $check
	test -f /usr/share/doc/tailbridge/copyright
	dpkg -s tailbridge | grep -q '^Depends: nftables'
	apt-get remove -y -qq tailbridge >/dev/null
	! test -e /usr/bin/tailbridge
"

run "deb on ubuntu" ubuntu:24.04 "
	apt-get update -qq >/dev/null
	apt-get install -y -qq /dist/$deb >/dev/null
	set -- /usr/bin/tailbridge /usr/lib/systemd/system/tailbridge.service; $check
	apt-get remove -y -qq tailbridge >/dev/null
	! test -e /usr/bin/tailbridge
"

run "rpm on fedora" fedora:latest "
	dnf install -y -q /dist/$rpm >/dev/null
	set -- /usr/bin/tailbridge /usr/lib/systemd/system/tailbridge.service; $check
	rpm -q --requires tailbridge | grep -q nftables
	test -f /usr/share/licenses/tailbridge/LICENSE
	dnf remove -y -q tailbridge >/dev/null
	! test -e /usr/bin/tailbridge
"

run "tarball + install.sh on alpine (no systemd)" alpine:latest "
	apk add -q nftables
	TAILBRIDGE_ARCHIVE=/dist/$tgz sh /install.sh
	set -- /usr/local/bin/tailbridge /etc/systemd/system/tailbridge.service; $check
	sh /install.sh --uninstall
	! test -e /usr/local/bin/tailbridge
	! test -e /etc/systemd/system/tailbridge.service
"

run "extracted tarball, offline install.sh on debian" debian:stable "
	mkdir /x && tar -xzf /dist/$tgz -C /x
	cd /x/* && ./install.sh
	set -- /usr/local/bin/tailbridge /etc/systemd/system/tailbridge.service; $check
	test -f /usr/local/share/doc/tailbridge/USAGE.md
	./install.sh --uninstall
	! test -e /usr/local/bin/tailbridge
"

exit $FAIL
