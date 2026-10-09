package remux

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"testing"
)

// installed returns where a program is, and skips the test if it is not
// installed.
func installed(t *testing.T, program string) string {
	t.Helper()
	path, err := exec.LookPath(program)
	if err != nil {
		t.Skip(program + " is not installed")
	}
	return path
}

// TestCopy remuxes a short generated HLS stream end to end with ffmpeg, through
// a relay and across a redirect.
func TestCopy(t *testing.T) {
	ffmpeg := installed(t, "ffmpeg")

	dir := t.TempDir()
	generate := exec.Command(ffmpeg, "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc=duration=2:size=160x120:rate=25",
		"-f", "lavfi", "-i", "sine=duration=2",
		"-c:v", "mpeg2video", "-c:a", "mp2",
		"-f", "hls", "-hls_time", "1", "-hls_playlist_type", "vod",
		filepath.Join(dir, "stream.m3u8"))
	if out, err := generate.CombinedOutput(); err != nil {
		t.Skipf("ffmpeg cannot generate a test stream: %v\n%s", err, out)
	}
	edge := httptest.NewServer(http.FileServer(http.Dir(dir)))
	defer edge.Close()
	router := httptest.NewServer(http.RedirectHandler(edge.URL+"/stream.m3u8", http.StatusFound))
	defer router.Close()

	remuxer, err := New()
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := remuxer.Copy(t.Context(), &out, Stream{Manifest: router.URL + "/live.m3u8", Client: router.Client()}); err != nil {
		t.Fatal(err)
	}

	// MPEG-TS is a run of 188-byte packets, each starting with the sync byte 0x47.
	if out.Len() == 0 || out.Len()%188 != 0 || out.Bytes()[0] != 0x47 {
		t.Errorf("Copy() wrote %d bytes that do not look like MPEG-TS", out.Len())
	}
}

// A stream that cannot be read must come to nothing, not even an empty write:
// the tuner takes the first write for the stream going on air.
func TestCopyOfNothing(t *testing.T) {
	remuxer, err := New()
	if err != nil {
		t.Skip("ffmpeg is not installed")
	}
	upstream := httptest.NewServer(http.NotFoundHandler())
	defer upstream.Close()

	var out writes
	if err := remuxer.Copy(t.Context(), &out, Stream{Manifest: upstream.URL + "/live.m3u8", Client: upstream.Client()}); err == nil || len(out) != 0 {
		t.Errorf("Copy() = %v after %d writes, want an error and no write", err, len(out))
	}
}
