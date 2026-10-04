// Package remux turns a live HTTP stream into the continuous MPEG-TS that Plex
// expects from a tuner.
//
// ffmpeg does the work as a stream copy: the container changes, audio and
// video are left untouched, so it costs next to no CPU. ffmpeg never talks to
// the network itself. It reads through a loopback relay, so every upstream
// request is made by the caller's own HTTP client and goes wherever that
// client goes, for instance through a proxy that ffmpeg could not use. The
// relay also repairs timestamps that ffmpeg would otherwise have to guess at.
//
// What ffmpeg writes is passed on from the point where all of its streams
// have started, so that the output opens with both picture and sound.
package remux

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"strconv"
)

// headStart is how many segments before its newest a live stream is joined
// at. They are all there to be fetched at once, so the viewer starts with that
// much of the stream in hand.
//
// It takes that much to keep Plex playing. A live stream arrives a segment at
// a time, seconds of it in one go, and ffmpeg looks for the next segment a
// little later every time round, until it is a whole segment behind and gets
// two at once. Plex holds a few seconds of what it receives back from its
// player. With ffmpeg's own choice of three segments, that left the player
// about a second from running dry on channels with segments of four seconds,
// and less after every segment.
const headStart = 6

// Remuxer remuxes streams to MPEG-TS.
type Remuxer struct {
	ffmpeg string // path to the ffmpeg binary
}

// New returns a Remuxer. It fails if ffmpeg is not installed.
func New() (*Remuxer, error) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		return nil, fmt.Errorf("remux: ffmpeg is required: %w", err)
	}
	return &Remuxer{ffmpeg: ffmpeg}, nil
}

// Copy remuxes the stream behind the manifest URL to MPEG-TS and writes it to
// w, fetching everything with client. It returns when the stream ends, ffmpeg
// fails, or ctx is cancelled.
func (r *Remuxer) Copy(ctx context.Context, w io.Writer, manifest string, client *http.Client) error {
	relay, local, err := openRelay(manifest, client)
	if err != nil {
		return fmt.Errorf("remux: %w", err)
	}
	defer relay.close()

	// Fatal errors only, unless debugging: ffmpeg tries every quality of a
	// stream before it settles on the best, and reports dropping the others
	// as errors.
	loglevel := "fatal"
	if slog.Default().Enabled(ctx, slog.LevelDebug) {
		loglevel = "warning"
	}
	args := []string{"-hide_banner", "-nostdin", "-loglevel", loglevel}
	if upstream, err := url.Parse(manifest); err == nil && path.Ext(upstream.Path) == ".m3u8" {
		// Only ffmpeg's HLS reader knows this option, and ffmpeg refuses
		// options it has no use for.
		args = append(args, "-live_start_index", strconv.Itoa(-headStart))
	}
	args = append(args,
		"-i", local,
		"-c", "copy", // change the container only: no transcoding
		"-f", "mpegts", "pipe:1",
	)
	cmd := exec.CommandContext(ctx, r.ffmpeg, args...)
	cmd.Stdout = newAligner(w)
	cmd.Stderr = os.Stderr
	// ffmpeg talks to the relay only, so a proxy from the environment must not
	// get in between.
	cmd.Env = append(os.Environ(), "no_proxy=*")
	return cmd.Run()
}
