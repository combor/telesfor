#!/bin/sh
# Upgrades pass "configure <old version>" for deb, a count of 2+ for rpm.
set -e

case "$1" in
configure) upgrade=$2 ;;
[2-9]*) upgrade=1 ;;
*) upgrade= ;;
esac

# A service that fails to restart must not fail the package install.
if [ -d /run/systemd/system ]; then
	systemctl daemon-reload || true
	if [ -n "$upgrade" ]; then
		systemctl try-restart telesfor.service || true
	fi
fi

if [ -z "$upgrade" ]; then
	echo "Set the proxies you need in /etc/telesfor/telesfor.env, then start telesfor with:"
	echo "  systemctl enable --now telesfor"
fi
