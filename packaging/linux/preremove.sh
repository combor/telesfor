#!/bin/sh
# Removal passes "remove" for deb, 0 for rpm. Leave upgrades running.
set -e

case "$1" in
remove | 0)
	if command -v systemctl >/dev/null 2>&1; then
		systemctl disable --now telesfor.service >/dev/null 2>&1 || true
	fi
	;;
esac
