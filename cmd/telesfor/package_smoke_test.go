package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

// distro is a Linux distribution that telesfor is packaged for.
type distro struct {
	name       string
	dockerfile string
	// Commands for the container. It has the snapshot at /dist and the
	// packaging files at /packaging; GOARCH selects the package architecture.
	install, upgrade, remove string
	// Arch leaves restarting an upgraded service to the operator.
	restarts bool
}

var distros = []distro{
	{
		name: "debian",
		dockerfile: `FROM debian:trixie-slim
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update && apt-get install -y --no-install-recommends systemd`,
		install:  "apt-get install -y /dist/telesfor_*_linux_$GOARCH.deb",
		upgrade:  "apt-get install -y --reinstall /dist/telesfor_*_linux_$GOARCH.deb",
		remove:   "apt-get remove -y telesfor",
		restarts: true,
	},
	{
		name: "fedora",
		dockerfile: `FROM fedora:44
RUN dnf install -y systemd`,
		install:  "dnf install -y /dist/telesfor_*_linux_$GOARCH.rpm",
		upgrade:  "dnf reinstall -y /dist/telesfor_*_linux_$GOARCH.rpm",
		remove:   "dnf remove -y telesfor",
		restarts: true,
	},
	{
		// The generated PKGBUILD is built, which checks its archive checksums.
		// A cached image may have a package database older than the mirrors.
		name: "arch",
		dockerfile: `FROM archlinux:latest
RUN pacman -Syu --noconfirm --needed binutils debugedit fakeroot && useradd -m builder`,
		install: `set -e
install -d -o builder /build
cd /build
cp /dist/aur/telesfor-bin.pkgbuild PKGBUILD
cp /packaging/telesfor.install .
. ./PKGBUILD
cp /dist/telesfor_*_linux_$GOARCH.tar.gz "${pkgname}_${pkgver}_$(uname -m).tar.gz"
chown builder ./*
runuser -u builder -- makepkg --nodeps
pacman -Syu --noconfirm
pacman -U --noconfirm telesfor-bin-*.pkg.tar.zst`,
		upgrade: "pacman -U --noconfirm /build/telesfor-bin-*.pkg.tar.zst",
		remove:  "systemctl disable --now telesfor && pacman -R --noconfirm telesfor-bin",
	},
}

// TestPackageService installs each Linux package in a container that runs
// systemd, and runs telesfor as the service it installs. Set
// TELESFOR_SMOKE_DIST to the dist directory of a GoReleaser snapshot.
//
// The service gets its channels from TVP as it starts, so the test needs the
// network.
func TestPackageService(t *testing.T) {
	dist := os.Getenv("TELESFOR_SMOKE_DIST")
	if dist == "" {
		t.Skip("TELESFOR_SMOKE_DIST is unset; no packages to test")
	}
	if !filepath.IsAbs(dist) {
		t.Fatalf("TELESFOR_SMOKE_DIST is %q, want an absolute path", dist)
	}
	packaging, err := filepath.Abs(filepath.Join("..", "..", "packaging", "linux"))
	if err != nil {
		t.Fatal(err)
	}

	// The remux tests run ffmpeg through a relay, as a tuned channel does. They
	// are run inside the unit's sandbox, which no real stream can be in CI.
	remuxTests := filepath.Join(t.TempDir(), "remux.test")
	build := exec.Command("go", "test", "-c", "-o", remuxTests, "github.com/combor/telesfor/internal/remux")
	build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building the remux tests: %v\n%s", err, out)
	}

	for _, d := range distros {
		t.Run(d.name, func(t *testing.T) {
			t.Parallel()
			testPackage(t, d, dist, packaging, remuxTests)
		})
	}
}

func testPackage(t *testing.T, d distro, dist, packaging, remuxTests string) {
	if d.name == "arch" && runtime.GOARCH != "amd64" {
		t.Skip("the Arch Linux image is amd64 only")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	image := "telesfor-package-smoke-" + d.name
	build := exec.CommandContext(ctx, "docker", "build", "-q", "-t", image, "-")
	build.Stdin = strings.NewReader(d.dockerfile)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building the image: %v\n%s", err, out)
	}
	// Privileged, so that systemd can boot and build the unit's sandbox.
	id := docker(ctx, t, "run", "-d", "--privileged", "--cgroupns=private",
		"--tmpfs", "/run", "--tmpfs", "/run/lock", "-p", "127.0.0.1::5004",
		"-e", "GOARCH="+runtime.GOARCH,
		"--mount", "type=bind,readonly,source="+dist+",target=/dist",
		"--mount", "type=bind,readonly,source="+packaging+",target=/packaging",
		image, "/usr/lib/systemd/systemd")
	c := container{ctx: ctx, id: id}
	t.Cleanup(func() {
		if t.Failed() {
			// ctx is done by now.
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			journal, _ := container{ctx: ctx, id: id}.try("journalctl -u telesfor --no-pager -o cat -n 100")
			t.Logf("telesfor journal:\n%s", journal)
		}
		_ = exec.Command("docker", "rm", "-f", id).Run()
	})
	c.await(t, "systemctl is-system-running", "running", "degraded")

	c.sh(t, d.install)
	// Starting it is left to the operator, who may have a proxy to set first.
	if state, _ := c.try("systemctl is-enabled telesfor"); state != "disabled" {
		t.Errorf("installed service is %s, want disabled", state)
	}

	docker(ctx, t, "cp", remuxTests, id+":/usr/local/bin/remux.test")
	c.sh(t, `mkdir -p /run/systemd/system/telesfor.service.d
printf '[Service]\nExecStart=\nExecStart=/usr/local/bin/remux.test -test.v\nRestart=no\n' >/run/systemd/system/telesfor.service.d/remux.conf
systemctl daemon-reload
systemctl start telesfor`)
	c.await(t, "systemctl show -p ActiveState --value telesfor", "inactive", "failed")
	log := c.sh(t, "journalctl --sync && journalctl -u telesfor --no-pager -o cat")
	if result := c.sh(t, "systemctl show -p Result --value telesfor"); result != "success" {
		t.Errorf("remux tests under the sandbox: %s\n%s", result, log)
	} else if !strings.Contains(log, "--- PASS: TestCopy") {
		t.Errorf("the test of ffmpeg did not run under the sandbox:\n%s", log)
	}
	c.sh(t, "rm -r /run/systemd/system/telesfor.service.d /usr/local/bin/remux.test && systemctl daemon-reload")

	c.sh(t, "systemctl enable --now telesfor")
	addr, _, _ := strings.Cut(docker(ctx, t, "port", id, "5004/tcp"), "\n")
	awaitLineup(ctx, t, "http://"+addr)

	pid := c.sh(t, "systemctl show -p MainPID --value telesfor")
	c.sh(t, d.upgrade)
	if got := c.sh(t, "systemctl show -p MainPID --value telesfor"); (got != pid) != d.restarts {
		t.Errorf("upgrade changed the main PID from %s to %s, want a restart: %v", pid, got, d.restarts)
	}
	awaitLineup(ctx, t, "http://"+addr)

	c.sh(t, d.remove)
	if state, _ := c.try("systemctl is-active telesfor"); state != "inactive" {
		t.Errorf("removed package's service is %s, want inactive", state)
	}
	c.sh(t, "test ! -e /etc/systemd/system/multi-user.target.wants/telesfor.service")
	c.sh(t, "test ! -e /usr/lib/systemd/system/telesfor.service")
}

// container is a running container to run shell scripts in.
type container struct {
	ctx context.Context
	id  string
}

func (c container) try(script string) (string, error) {
	out, err := exec.CommandContext(c.ctx, "docker", "exec", c.id, "sh", "-c", script).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func (c container) sh(t *testing.T, script string) string {
	t.Helper()
	out, err := c.try(script)
	if err != nil {
		t.Fatalf("%s: %v\n%s", script, err, out)
	}
	return out
}

// await waits until the script prints one of the wanted lines.
func (c container) await(t *testing.T, script string, want ...string) {
	t.Helper()
	deadline := time.After(2 * time.Minute)
	for {
		out, _ := c.try(script)
		if slices.Contains(want, out) {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("%s printed %q, want one of %q", script, out, want)
		case <-time.After(200 * time.Millisecond):
		}
	}
}
