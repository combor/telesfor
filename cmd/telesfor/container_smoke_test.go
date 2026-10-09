package main

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestContainerServesLineup starts the container image and asks it for its
// lineup. Set TELESFOR_SMOKE_IMAGE to the image to test.
//
// The container gets its channels from TVP as it starts, so the test needs the
// network. TVP's channel list is open to the world.
func TestContainerServesLineup(t *testing.T) {
	image := os.Getenv("TELESFOR_SMOKE_IMAGE")
	if image == "" {
		t.Skip("TELESFOR_SMOKE_IMAGE is unset; no image to test")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	// No --rm, so that a container that exits as it starts keeps its logs.
	id := docker(ctx, t, "run", "-d", "-p", "127.0.0.1::5004", image)
	t.Cleanup(func() {
		if t.Failed() {
			logs, _ := exec.Command("docker", "logs", id).CombinedOutput()
			health, _ := exec.Command("docker", "inspect", "-f", "{{json .State.Health.Log}}", id).Output()
			t.Logf("container logs:\n%s\nhealth checks: %s", logs, health)
		}
		_ = exec.Command("docker", "rm", "-f", id).Run()
	})
	addr, _, _ := strings.Cut(docker(ctx, t, "port", id, "5004/tcp"), "\n")
	awaitLineup(ctx, t, "http://"+addr)

	for {
		status := docker(ctx, t, "inspect", "-f", "{{.State.Health.Status}}", id)
		if status == "healthy" {
			break
		}
		if status == "unhealthy" {
			t.Fatalf("health status %q", status)
		}
		time.Sleep(time.Second)
	}

	// telesfor found ffmpeg, or it would not have started. Running it also
	// checks that the libraries it needs are in the image.
	if out := docker(ctx, t, "exec", id, "ffmpeg", "-hide_banner", "-version"); !strings.HasPrefix(out, "ffmpeg version") {
		t.Errorf("ffmpeg -version printed %q", out)
	}
}
