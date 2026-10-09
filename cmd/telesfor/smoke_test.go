package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// awaitLineup waits for the telesfor at base to serve its lineup, and checks
// that it is TVP's.
func awaitLineup(ctx context.Context, t *testing.T, base string) {
	t.Helper()
	wait, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()

	var lineup []struct{ GuideNumber, GuideName, URL string }
	for {
		err := getJSON(wait, base+"/lineup.json", &lineup)
		if err == nil {
			break
		}
		select {
		case <-wait.Done():
			t.Fatalf("the lineup never came: %v", err)
		case <-time.After(200 * time.Millisecond):
		}
	}
	if len(lineup) == 0 || !strings.HasPrefix(lineup[0].URL, base+"/stream/tvp/") {
		t.Errorf("lineup = %+v, want TVP's channels, streamed from %s", lineup, base)
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
