#!/bin/sh
# Shared by the .deb and .rpm packages.
# deb passes "remove"/"upgrade"/...; rpm passes the number of versions left
# after the transaction (0 = uninstall, 1 = upgrade).
case "$1" in
	remove | purge | 0) ;;
	*) exit 0 ;;
esac
if [ -d /run/systemd/system ]; then
	systemctl disable --now tailbridge.service >/dev/null 2>&1 || true
fi
# Remove only Tailbridge-owned aliases, firewall table and advertised routes.
# The configuration in /etc/tailbridge is kept.
if [ -x /usr/bin/tailbridge ]; then
	/usr/bin/tailbridge down || echo "tailbridge: 'tailbridge down' failed; run it manually to remove leftover aliases/rules" >&2
fi
exit 0
