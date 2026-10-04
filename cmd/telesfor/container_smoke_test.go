package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
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
			t.Logf("container logs:\n%s", logs)
		}
		_ = exec.Command("docker", "rm", "-f", id).Run()
	})
	addr, _, _ := strings.Cut(docker(ctx, t, "port", id, "5004/tcp"), "\n")
	base := "http://" + addr

	var lineup []struct{ GuideNumber, GuideName, URL string }
	for {
		err := getJSON(ctx, base+"/lineup.json", &lineup)
		if err == nil {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("the lineup never came: %v", err)
		case <-time.After(200 * time.Millisecond):
		}
	}
	if len(lineup) == 0 || !strings.HasPrefix(lineup[0].URL, base+"/stream/tvp/") {
		t.Errorf("lineup = %+v, want TVP's channels, streamed from %s", lineup, base)
	}

	for {
		status := docker(ctx, t, "inspect", "-f", "{{.State.Health.Status}}", id)
		if status == "healthy" {
			break
		}
		if status == "unhealthy" || ctx.Err() != nil {
			t.Fatalf("health status %q: %s", status, docker(ctx, t, "inspect", "-f", "{{json .State.Health.Log}}", id))
		}
		time.Sleep(time.Second)
	}

	// telesfor found ffmpeg, or it would not have started. Running it also
	// checks that the libraries it needs are in the image.
	if out := docker(ctx, t, "exec", id, "ffmpeg", "-hide_banner", "-version"); !strings.HasPrefix(out, "ffmpeg version") {
		t.Errorf("ffmpeg -version printed %q", out)
	}
}

// docker runs a docker command and returns what it printed.
func docker(ctx context.Context, t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.CommandContext(ctx, "docker", args...).Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			t.Fatalf("docker %s: %v\n%s", args[0], err, exitErr.Stderr)
		}
		t.Fatalf("docker %s: %v", args[0], err)
	}
	return strings.TrimSpace(string(out))
}

// getJSON fetches a URL and decodes its JSON response into v.
func getJSON(ctx context.Context, url string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return errors.New(resp.Status)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	return json.Unmarshal(body, v)
}
