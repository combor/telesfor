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
// have started, so that the output opens with both picture and sound, and
// once enough of it has come to keep Plex playing.
//
// A stream that comes in more than one quality is played in the best that the
// connection keeps up with, which may change as it plays: see controller. It
// is one ffmpeg after another then, each on a quality: see stage.
package remux

import (
	"cmp"
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
	"sync"
	"time"
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

	hold, calm time.Duration // see plexHold and calm; tests are in more of a hurry

	mu     sync.Mutex
	routes map[string]*route // what is known of the connections streams come over, by their names
}

// Stream is a live stream to remux.
type Stream struct {
	Manifest string       // the URL of its manifest
	Client   *http.Client // fetches everything of it
	Name     string       // what to call it in the log: the channel's name
	Route    string       // names the connection it comes over: streams of one route share its speed
}

// New returns a Remuxer. It fails if ffmpeg is not installed.
func New() (*Remuxer, error) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		return nil, fmt.Errorf("remux: ffmpeg is required: %w", err)
	}
	return &Remuxer{ffmpeg: ffmpeg, hold: plexHold, calm: calm, routes: map[string]*route{}}, nil
}

// route returns what is known of the connection of a name.
func (r *Remuxer) route(name string) *route {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.routes[name] == nil {
		r.routes[name] = &route{}
	}
	return r.routes[name]
}

// Copy remuxes a stream to MPEG-TS and writes it to w. It returns when the
// stream ends, ffmpeg fails, or ctx is cancelled.
func (r *Remuxer) Copy(ctx context.Context, w io.Writer, s Stream) error {
	relay, local, err := openRelay(s.Manifest, s.Client)
	if err != nil {
		return fmt.Errorf("remux: %w", err)
	}
	defer relay.close()

	// Only ffmpeg's HLS reader knows where a live stream is joined, and only
	// HLS comes in qualities to choose from.
	upstream, err := url.Parse(s.Manifest)
	if err != nil || path.Ext(upstream.Path) != ".m3u8" {
		return r.single(ctx, w, relay, local) // ffmpeg refuses options it has no use for
	}
	if qualities := relay.qualities(ctx, s.Manifest); qualities != nil {
		return r.play(ctx, w, s, relay, qualities)
	}
	return r.single(ctx, w, relay, local, "-live_start_index", strconv.Itoa(-headStart))
}

// single remuxes a stream that has no qualities to choose from.
func (r *Remuxer) single(ctx context.Context, w io.Writer, relay *relay, local string, options ...string) error {
	out := newReserve(w, relay.short.Load)
	cmd := r.command(ctx, local, options...)
	cmd.Stdout = newAligner(out)
	err := cmd.Run()
	if ctx.Err() == nil { // not to a viewer who has left
		err = cmp.Or(err, out.flush())
	}
	return err
}

// play remuxes a stream that comes in more than one quality: a leg at a time,
// each read by an ffmpeg of its own. See stage.
func (r *Remuxer) play(ctx context.Context, w io.Writer, s Stream, relay *relay, qualities *ladder) (err error) {
	gauge := newMeter(w)
	ctl := newController(s.Name, qualities, r.route(s.Route), gauge.sent)
	ctl.hold, ctl.calm = r.hold, r.calm
	defer ctl.end()
	out := newReserve(gauge, relay.short.Load)
	splice := newSplicer(newAligner(out))
	defer func() {
		if ctx.Err() == nil { // not to a viewer who has left
			err = cmp.Or(err, out.flush())
		}
	}()

	st, err := openStage(relay, qualities, ctl, gauge, func(l *leg, input string) error {
		return r.launch(ctx, l, input)
	})
	if err != nil {
		return fmt.Errorf("remux: %w", err)
	}
	defer st.close()

	l, err := st.begin(ctl.start())
	if err != nil {
		return fmt.Errorf("remux: starting ffmpeg: %w", err)
	}
	for {
		n, err := io.Copy(splice, l.out)
		if err != nil {
			l.stop() // its ffmpeg would wait for somebody to take what it writes
		}
		ended := l.wait()
		l.stop()
		slog.Debug("remux: ffmpeg ended", "leg", l.n, "quality", qualities.qualities[l.quality].String(), "bytes", n)
		if err != nil || ctx.Err() != nil {
			return cmp.Or(err, ended)
		}
		// How the ffmpeg of a leg ended matters only if the stream fails with it.
		if l, err = st.after(l, n > 0, ended); l == nil {
			return err
		}
		if l.startsOver {
			// What the ffmpeg before had written has gone nowhere: the
			// stream starts over.
			out = newReserve(gauge, relay.short.Load)
			splice = newSplicer(newAligner(out))
			gauge.started()
		} else {
			splice.next()
		}
	}
}

func (r *Remuxer) launch(ctx context.Context, l *leg, input string) error {
	ctx, stop := context.WithCancel(ctx)
	// The stage has each leg begin with its first segment.
	cmd := r.command(ctx, input, "-live_start_index", "0")
	written, err := cmd.StdoutPipe()
	if err == nil {
		err = cmd.Start()
	}
	if err != nil {
		stop()
		return err
	}
	l.stop, l.out, l.wait = stop, written, sync.OnceValue(cmd.Wait)
	return nil
}

// command returns the ffmpeg that remuxes what it reads at an address to
// MPEG-TS on its standard output, with options for how it reads.
func (r *Remuxer) command(ctx context.Context, input string, options ...string) *exec.Cmd {
	// Fatal errors only, unless debugging: ffmpeg tries every quality of a
	// stream before it settles on the best, and reports dropping the others
	// as errors.
	loglevel := "fatal"
	if slog.Default().Enabled(ctx, slog.LevelDebug) {
		loglevel = "warning"
	}
	args := append([]string{"-hide_banner", "-nostdin", "-loglevel", loglevel}, options...)
	args = append(args,
		"-i", input,
		"-c", "copy", // change the container only: no transcoding
		"-f", "mpegts", "pipe:1",
	)
	cmd := exec.CommandContext(ctx, r.ffmpeg, args...)
	cmd.Stderr = os.Stderr
	// ffmpeg talks to the relay only, so a proxy from the environment must not
	// get in between.
	cmd.Env = append(os.Environ(), "no_proxy=*")
	return cmd
}
