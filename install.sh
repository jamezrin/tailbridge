#!/bin/sh
# Install (or uninstall) Tailbridge on any systemd-based Linux distribution.
#
#   curl -fsSL https://raw.githubusercontent.com/jamezrin/tailbridge/main/install.sh | sudo sh
#   sudo ./install.sh               # from an extracted release archive (offline)
#   sudo ./install.sh --uninstall
#
# Environment:
#   TAILBRIDGE_VERSION   release to install, e.g. v0.1.0 (default: latest)
#   PREFIX               install prefix (default: /usr/local)
#   TAILBRIDGE_ARCHIVE   install from this local release archive instead of downloading
set -eu

REPO=jamezrin/tailbridge
PREFIX=${PREFIX:-/usr/local}
UNIT_DIR=/etc/systemd/system

say() { printf 'tailbridge-install: %s\n' "$*"; }
die() { printf 'tailbridge-install: error: %s\n' "$*" >&2; exit 1; }

[ "$(uname -s)" = Linux ] || die "Tailbridge only supports Linux"
[ "$(id -u)" -eq 0 ] || die "run as root (e.g. with sudo)"

uninstall() {
	if [ -d /run/systemd/system ]; then
		systemctl disable --now tailbridge.service >/dev/null 2>&1 || true
	fi
	if [ -x "$PREFIX/bin/tailbridge" ]; then
		"$PREFIX/bin/tailbridge" down || say "warning: 'tailbridge down' failed; leftover aliases/rules may remain"
	fi
	rm -f "$PREFIX/bin/tailbridge" "$UNIT_DIR/tailbridge.service"
	rm -rf "$PREFIX/share/doc/tailbridge" "$PREFIX/share/licenses/tailbridge"
	[ -d /run/systemd/system ] && systemctl daemon-reload || true
	say "uninstalled (configuration in /etc/tailbridge and state in /var/lib/tailbridge were kept)"
}

case "${1:-}" in
	--uninstall) uninstall; exit 0 ;;
	"") ;;
	*) die "usage: install.sh [--uninstall]" ;;
esac

fetch() { # url dest
	if command -v curl >/dev/null 2>&1; then
		curl -fsSL -o "$2" "$1"
	elif command -v wget >/dev/null 2>&1; then
		wget -qO "$2" "$1"
	else
		die "need curl or wget"
	fi
}

latest_version() {
	# Follow the /releases/latest redirect; avoids GitHub API rate limits.
	if command -v curl >/dev/null 2>&1; then
		url=$(curl -fsSLI -o /dev/null -w '%{url_effective}' "https://github.com/$REPO/releases/latest")
	else
		url=$(wget -S --spider "https://github.com/$REPO/releases/latest" 2>&1 | sed -n 's/^ *[Ll]ocation: //p' | tail -n1)
	fi
	v=${url##*/}
	case "$v" in v*) echo "$v" ;; *) die "could not determine the latest release" ;; esac
}

case "$(uname -m)" in
	x86_64 | amd64) ARCH=amd64 ;;
	aarch64 | arm64) ARCH=arm64 ;;
	armv7l | armv7*) ARCH=armv7 ;;
	*) die "unsupported architecture $(uname -m)" ;;
esac

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

script_dir=$(cd "$(dirname "$0")" 2>/dev/null && pwd || echo .)
if [ -n "${TAILBRIDGE_ARCHIVE:-}" ]; then
	tar -xzf "$TAILBRIDGE_ARCHIVE" -C "$TMP"
	SRC=$(find "$TMP" -maxdepth 2 -type f -name tailbridge -exec dirname {} \; | head -n1)
elif [ -f "$script_dir/tailbridge" ] && [ -f "$script_dir/tailbridge.service" ]; then
	SRC=$script_dir
else
	VERSION=${TAILBRIDGE_VERSION:-$(latest_version)}
	case "$VERSION" in v*) ;; *) VERSION=v$VERSION ;; esac
	NAME="tailbridge_${VERSION#v}_linux_${ARCH}"
	BASE="https://github.com/$REPO/releases/download/$VERSION"
	say "downloading $NAME.tar.gz ($VERSION)"
	fetch "$BASE/$NAME.tar.gz" "$TMP/$NAME.tar.gz"
	fetch "$BASE/checksums.txt" "$TMP/checksums.txt"
	(cd "$TMP" && grep " $NAME.tar.gz\$" checksums.txt | sha256sum -c - >/dev/null) ||
		die "checksum verification failed for $NAME.tar.gz"
	tar -xzf "$TMP/$NAME.tar.gz" -C "$TMP"
	SRC="$TMP/$NAME"
fi
[ -n "${SRC:-}" ] && [ -f "$SRC/tailbridge" ] || die "release archive does not contain a tailbridge binary"

install -Dm755 "$SRC/tailbridge" "$PREFIX/bin/tailbridge"
sed "s|^ExecStart=.*/tailbridge |ExecStart=$PREFIX/bin/tailbridge |" "$SRC/tailbridge.service" >"$TMP/tailbridge.service"
install -Dm644 "$TMP/tailbridge.service" "$UNIT_DIR/tailbridge.service"
for f in README.md USAGE.md; do
	[ -f "$SRC/$f" ] && install -Dm644 "$SRC/$f" "$PREFIX/share/doc/tailbridge/$f"
done
[ -f "$SRC/LICENSE" ] && install -Dm644 "$SRC/LICENSE" "$PREFIX/share/licenses/tailbridge/LICENSE"

if [ -d /run/systemd/system ]; then
	systemctl daemon-reload
	if systemctl is-active --quiet tailbridge.service; then
		systemctl restart tailbridge.service
		say "restarted the running tailbridge service"
	fi
fi

say "installed $("$PREFIX/bin/tailbridge" --version) to $PREFIX/bin"
command -v nft >/dev/null 2>&1 || say "warning: nft (nftables) not found; install it with your package manager"
command -v tailscaled >/dev/null 2>&1 || say "warning: tailscaled not found; see https://tailscale.com/download/linux"
cat <<'MSG'
Next steps:
  sudo tailbridge diagnose
  sudo systemctl enable --now tailbridge
MSG
