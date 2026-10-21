#!/bin/sh
# Shared by the .deb and .rpm packages.
if [ -d /run/systemd/system ]; then
	systemctl daemon-reload >/dev/null 2>&1 || true
	# On upgrade, restart the daemon if it was running.
	if systemctl is-active --quiet tailbridge.service; then
		systemctl restart tailbridge.service || true
	fi
fi
cat <<'MSG'
tailbridge installed. Next steps:
  sudo tailbridge diagnose
  sudo systemctl enable --now tailbridge
Docs: /usr/share/doc/tailbridge/USAGE.md
MSG
exit 0
