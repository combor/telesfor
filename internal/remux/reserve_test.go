package remux

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// writes notes what it is given, one piece at a time.
type writes [][]byte

func (w *writes) Write(p []byte) (int, error) {
	*w = append(*w, bytes.Clone(p))
	return len(p), nil
}

// TestReserve holds a stream that ffmpeg wrote back until enough of it has
// come.
func TestReserve(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg is not installed")
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe is not installed")
	}
	stream, err := exec.Command(ffmpeg, "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc=duration=12:size=160x120:rate=25",
		"-f", "lavfi", "-i", "sine=duration=12",
		"-c:v", "mpeg2video", "-c:a", "mp2",
		"-f", "mpegts", "pipe:1").Output()
	if err != nil {
		t.Skipf("ffmpeg cannot generate a test stream: %v", err)
	}

	short := true
	var out writes
	held := newReserve(&out, func() bool { return short })
	for piece := range slices.Chunk(stream, 1000) { // pieces that end anywhere, not where packets end
		held.Write(piece)
	}
	if len(out) < 2 || !bytes.Equal(slices.Concat(out...), stream) {
		t.Fatalf("the stream was passed on in %d pieces, want all of it: its start in one, and the rest as it came", len(out))
	}

	// ffprobe tells the length of a file only.
	start := filepath.Join(t.TempDir(), "start.ts")
	if err := os.WriteFile(start, out[0], 0o600); err != nil {
		t.Fatal(err)
	}
	length, err := exec.Command(ffprobe, "-hide_banner", "-loglevel", "error",
		"-show_entries", "format=duration", "-of", "csv=p=0", start).Output()
	if err != nil {
		t.Fatal(err)
	}
	if seconds, err := strconv.ParseFloat(strings.TrimSpace(string(length)), 64); err != nil || seconds < minLead.Seconds() || seconds > minLead.Seconds()+0.5 {
		t.Errorf("the stream started with %q seconds of it, want just over %v", length, minLead)
	}

	// A stream whose playlist gave it its head start has nothing to wait for.
	short, out = false, nil
	newReserve(&out, func() bool { return short }).Write(stream[:1000])
	if len(out) != 1 {
		t.Errorf("a stream with a playlist long enough was passed on %d times, want it at once", len(out))
	}
}

// What tells nothing about how much of it has come is not held back, and a
// stream that ends early is sent when it does.
func TestReserveGivesUp(t *testing.T) {
	short := func() bool { return true }
	var out writes
	newReserve(&out, short).Write(bytes.Repeat([]byte("not MPEG-TS "), 100))
	if len(out) != 1 {
		t.Errorf("what is not MPEG-TS was passed on %d times, want it at once", len(out))
	}

	out = nil
	held := newReserve(&out, short)
	if err := held.flush(); err != nil || len(out) != 0 {
		t.Errorf("flush() = %v and passed on %d pieces of a stream that never came, want nothing", err, len(out))
	}
	held.Write(key1)
	if len(out) != 0 {
		t.Fatal("a stream was passed on with one packet of it in hand")
	}
	if err := held.flush(); err != nil || len(out) != 1 || !bytes.Equal(out[0], key1) {
		t.Errorf("flush() = %v and passed on %d pieces, want the packet that was held", err, len(out))
	}
}
