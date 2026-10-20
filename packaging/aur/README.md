# AUR packaging

`tailbridge-git/` builds Tailbridge from the latest commit on `main`:

- installs `/usr/bin/tailbridge`, the systemd unit and the docs
- runs the unit tests in `check()`
- builds with Arch's stock `go` and `GOTOOLCHAIN=local`, so nothing is downloaded at build time besides Go modules

## Before publishing

1. The GitHub repository must be public, or AUR users cannot clone the source.
2. Add a LICENSE file and set `license=()` to its SPDX identifier. namcap rejects `unknown`.

## Test locally

```sh
cd packaging/aur/tailbridge-git
makepkg -si          # build, run tests, install
makepkg --printsrcinfo > .SRCINFO
namcap PKGBUILD *.pkg.tar.zst
```

## Publish

```sh
git clone ssh://aur@aur.archlinux.org/tailbridge-git.git aur-tailbridge-git
cp packaging/aur/tailbridge-git/{PKGBUILD,.SRCINFO} aur-tailbridge-git/
cd aur-tailbridge-git && git add PKGBUILD .SRCINFO && git commit -m "Initial import" && git push
```

## Using it

```sh
sudo systemctl enable --now tailbridge
sudo tailbridge diagnose
```
