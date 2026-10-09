package remux

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"
)

const playlistType = "application/vnd.apple.mpegurl"

// stage plays a stream that comes in more than one quality to ffmpeg, one
// quality at a time.
//
// ffmpeg cannot change quality within a stream: the segments of another
// quality make no sense to it after those it has read. So the stream is
// played in legs. A leg is one quality from one segment to another, read by
// an ffmpeg of its own that knows of nothing else: the stage writes the
// playlists it reads, with that quality alone, beginning at the leg's first
// segment. To change quality, the stage ends the leg's playlists where ffmpeg
// has got to, which makes ffmpeg write out what it has and stop, and starts
// the next leg at the segment after. The splicer joins what the two write.
//
// Every address in those playlists leads back to the stage. Segments use the
// provider's HTTP/1.1 pool; playlists and keys use its ordinary client. The
// controller measures audio and video together. A slow video segment can be
// given up, and the next leg begins with that segment in a lower quality.
type stage struct {
	relay  *relay
	ladder *ladder
	ctl    *controller
	gauge  *meter                           // the stream on its way to the viewer
	start  func(l *leg, input string) error // starts the ffmpeg that reads a leg from input
	server *http.Server
	flow   flow // how fast audio and video have been coming

	// The controller's and the meter's locks are taken inside mu, never the
	// other way round.
	mu      sync.Mutex
	legs    []*leg         // by their numbers, from 1; nil once nothing is to come of one
	current *leg           // the one whose ffmpeg is being listened to
	pending *leg           // the one to follow it, once a change is planned
	over    bool           // the stream has ended, and a leg found nothing left of it
	closed  bool           // the stage has stopped: no leg is to follow
	keys    []*url.URL     // the keys and init sections of the playlists, by the numbers ffmpeg asks for them with
	numbers map[string]int // those numbers, by address
}

// openStage starts a stage for the stream that a relay is for.
func openStage(r *relay, l *ladder, ctl *controller, gauge *meter, start func(l *leg, input string) error) (*stage, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	s := &stage{relay: r, ladder: l, ctl: ctl, gauge: gauge, start: start, numbers: map[string]int{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/{leg}/master.m3u8", s.master)
	mux.HandleFunc("/{leg}/video.m3u8", func(w http.ResponseWriter, req *http.Request) { s.playlist(w, req, true) })
	mux.HandleFunc("/{leg}/audio.m3u8", func(w http.ResponseWriter, req *http.Request) { s.playlist(w, req, false) })
	mux.HandleFunc("/{leg}/v/{seq}/{file}", func(w http.ResponseWriter, req *http.Request) { s.segment(w, req, true) })
	mux.HandleFunc("/{leg}/a/{seq}/{file}", func(w http.ResponseWriter, req *http.Request) { s.segment(w, req, false) })
	mux.HandleFunc("/{leg}/x/{key}/{file}", s.key)
	s.server = &http.Server{Addr: listener.Addr().String(), Handler: mux} // the address is only kept to build URLs from
	go s.server.Serve(listener)
	return s, nil
}

// close stops the stage, and whatever ffmpeg still reads from it. It waits
// for each to end: that of a leg that was still to follow has nobody else to.
func (s *stage) close() {
	s.server.Close()
	s.mu.Lock()
	s.closed = true
	legs := s.legs
	s.mu.Unlock()
	for _, l := range legs {
		if l == nil {
			continue
		}
		l.drop()
		if l.stop != nil {
			l.stop()
			l.wait()
		}
	}
}

// leg looks up the leg that a request is for.
func (s *stage) leg(req *http.Request) *leg {
	n, _ := strconv.Atoi(req.PathValue("leg"))
	s.mu.Lock()
	defer s.mu.Unlock()
	if n < 1 || n > len(s.legs) {
		return nil
	}
	return s.legs[n-1]
}

// master answers for the master playlist of a leg: its quality and the sound.
func (s *stage) master(w http.ResponseWriter, req *http.Request) {
	l := s.leg(req)
	if l == nil {
		http.NotFound(w, req)
		return
	}
	w.Header().Set("Content-Type", playlistType)
	fmt.Fprintf(w, "#EXTM3U\n"+
		"#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"sound\",NAME=\"sound\",DEFAULT=YES,AUTOSELECT=YES,URI=\"audio.m3u8\"\n"+
		"#EXT-X-STREAM-INF:BANDWIDTH=%d,AUDIO=\"sound\"\nvideo.m3u8\n", s.ladder.qualities[l.quality].rate)
}

// playlist answers for the playlist of a leg's picture or sound: the one
// upstream, from where the leg begins or ffmpeg has got to, and no further
// than where the leg ends.
func (s *stage) playlist(w http.ResponseWriter, req *http.Request, video bool) {
	l := s.leg(req)
	if l == nil || (!video && s.ladder.sound == nil) {
		http.NotFound(w, req)
		return
	}
	t, address, letter := l.track(video), s.ladder.sound, "a"
	if video {
		address, letter = s.ladder.qualities[l.quality].playlist, "v"
	}
	// ffmpeg does without a playlist that it cannot read when it starts, and
	// plays what the other has: the picture without its sound, or the sound
	// alone. A leg that cannot begin with both is stopped instead, to be
	// followed by one that can: see after.
	fail := func(err error) {
		s.mu.Lock()
		begun := t.asked >= max(t.from, 0)
		s.mu.Unlock()
		if !begun {
			l.stop()
		}
		if err != nil {
			refuse(w, req, file(address), err)
		} else {
			http.NotFound(w, req)
		}
	}

	// A leg that has all it ends with reads no more of the stream. One that
	// begins waits for its first segment to be listed.
	for waited := time.Duration(0); ; {
		s.mu.Lock()
		complete := t.until >= 0 && t.list.next() >= t.until
		s.mu.Unlock()
		if complete {
			break
		}
		list, err := s.read(req.Context(), address)
		if err != nil {
			fail(err)
			return
		}
		s.mu.Lock()
		placed := s.place(l, list, video)
		if placed {
			t.list = list
		}
		ready := t.until >= 0 || t.from < list.next()
		if placed && !ready && list.ended {
			s.over = true
		}
		over := s.over
		s.mu.Unlock()
		if over {
			fail(nil)
			return
		}
		if !placed {
			slog.Warn("relay: a quality's segments cannot be told from those of the one before", "file", file(address))
			fail(nil)
			return
		}
		if ready {
			break
		}
		if waited >= 3*list.target {
			slog.Warn("relay: a quality's playlist does not get to where the one before stopped", "file", file(address))
			fail(nil)
			return
		}
		pause := min(max(list.target/4, 250*time.Millisecond), time.Second)
		select {
		case <-req.Context().Done():
			return
		case <-time.After(pause):
			waited += pause
		}
	}

	s.mu.Lock()
	text := s.rewrite(t, letter)
	s.mu.Unlock()
	w.Header().Set("Content-Type", playlistType)
	io.WriteString(w, text)
}

// rewrite writes the playlist of a track as ffmpeg is given it. The caller
// holds the lock.
func (s *stage) rewrite(t *track, letter string) string {
	// ffmpeg asks for a segment while it still reads the one before, and
	// skips what it reads if the playlist no longer lists it.
	from := max(t.from, t.asked-1)
	if t.until >= 0 {
		from = max(t.from, min(t.asked, t.until-1)-1)
	}
	return t.list.write(from, t.until, max(t.from, 0), t.until >= 0 && t.list.next() >= t.until, func(kind string, seq int64, uri string) string {
		u, err := url.Parse(uri)
		if err != nil {
			return uri
		}
		if kind == "segment" {
			return fmt.Sprintf("%s/%d/%s", letter, seq, url.PathEscape(file(u)))
		}
		n, known := s.numbers[uri]
		if !known {
			n, s.keys = len(s.keys), append(s.keys, u)
			s.numbers[uri] = n
		}
		return fmt.Sprintf("x/%d/%s", n, url.PathEscape(file(u)))
	})
}

// read fetches a playlist.
func (s *stage) read(ctx context.Context, address *url.URL) (playlist, error) {
	began := time.Now()
	resp, err := ask(ctx, s.relay.direct, address.String(), "")
	if err != nil {
		return playlist{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return playlist{}, refusal(resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxPlaylist))
	if err != nil {
		return playlist{}, err
	}
	slog.Debug("relay: fetched", "file", file(address), "status", resp.StatusCode, "bytes", len(body), "took", time.Since(began).Round(time.Millisecond))
	return parsePlaylist(string(body), resp.Request.URL) // after redirects
}

// segment answers for a segment of a leg's picture or sound.
//
// A segment of the picture is watched as it comes, unless it is of the lowest
// quality: the controller may say to give it up. Before the head start is
// in, ffmpeg gets it as it comes, like any other file: nothing has reached
// the viewer by then, and a stream that turns out to be too much is started
// over. After that, ffmpeg gets none of a segment before all of it has come,
// so that one given up has left no trace: ffmpeg is told there is none.
func (s *stage) segment(w http.ResponseWriter, req *http.Request, video bool) {
	l := s.leg(req)
	seq, err := strconv.ParseInt(req.PathValue("seq"), 10, 64)
	if l == nil || err != nil {
		http.NotFound(w, req)
		return
	}
	t := l.track(video)
	s.mu.Lock()
	wanted, listed := t.list.find(seq)
	if listed = listed && (t.until < 0 || seq < t.until); listed {
		t.asked = max(t.asked, seq)
	}
	newest := seq >= t.list.next()-1 // ffmpeg has caught up with the stream
	watched := video && l.quality > 0
	s.mu.Unlock()
	if !listed {
		http.NotFound(w, req)
		return
	}
	// give notes that ffmpeg is to get the segment now, unless the leg has
	// ended before it meanwhile.
	give := func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		if t.until >= 0 && seq >= t.until {
			return false
		}
		t.started = max(t.started, seq)
		if video {
			s.ctl.gives(wanted.length)
		}
		return true
	}
	whole := watched && s.ctl.holds()

	// A segment that is given up is fetched no further. One that ffmpeg is
	// getting as it comes is past that: it comes to its end, or ffmpeg goes.
	ctx, cancel := context.WithCancel(req.Context())
	defer cancel()
	keep := func() bool { return true }
	if whole {
		keep = context.AfterFunc(l.flying, cancel)
		defer keep()
	}
	asked := time.Now()
	resp, err := ask(ctx, s.relay.segments, wanted.uri.String(), req.Header.Get("Range"))
	if err != nil {
		if whole && l.flying.Err() != nil {
			http.NotFound(w, req) // given up
			return
		}
		refuse(w, req, file(wanted.uri), err)
		return
	}
	body := &counted{ReadCloser: resp.Body, flow: &s.flow, asked: asked, of: resp.ContentLength}
	if watched {
		body.own = &flow{}
	}
	resp.Body = body

	var size int64
	var came time.Time // when the last of it had come
	spool, fetched := newSpool(), make(chan struct{})
	go func() {
		defer close(fetched)
		var err error
		size, err = s.relay.pass(spool, resp)
		came = time.Now()
		spool.close(err)
	}()
	defer func() { // not before the fetch is over, which is still reading
		cancel()
		spool.close(io.ErrClosedPipe)
		<-fetched
		body.Close()
	}()
	if watched {
		go s.watch(l, body, wanted.length, fetched)
	}

	if whole = whole && resp.ContentLength > 0 && resp.ContentLength <= maxSegment; whole {
		if err := spool.wait(); err != nil || !give() {
			if err != nil && l.flying.Err() == nil && req.Context().Err() == nil {
				slog.Warn("relay: upstream transfer failed", "file", file(wanted.uri), "err", err)
			}
			http.NotFound(w, req) // nothing of it has reached ffmpeg, which goes on to the next
			return
		}
	} else if !keep() || !give() { // given up, or the leg ends before it
		http.NotFound(w, req)
		return
	}
	passHeaders(w, resp)
	if err := spool.passOn(w); err != nil {
		if video {
			s.ctl.lost(wanted.length)
		}
		if req.Context().Err() == nil && l.flying.Err() == nil {
			slog.Warn("relay: upstream transfer failed", "file", file(wanted.uri), "err", err)
		}
		panic(http.ErrAbortHandler) // see relay.fetch
	}
	<-fetched
	slog.Debug("relay: fetched", "file", file(wanted.uri), "status", resp.StatusCode, "bytes", size, "took", came.Sub(asked).Round(time.Millisecond))
	if !video {
		return
	}
	how := arrival{size: size, first: body.first, wait: came.Sub(asked), length: wanted.length, newest: newest}
	if !body.began.IsZero() {
		how.wait, how.flow = body.began.Sub(asked), came.Sub(body.began)
		how.fastest, _ = s.flow.fastest(body.began, came)
	}
	if to, why, ok := s.ctl.fetched(l.quality, how); ok {
		s.change(l, to, why)
	}
}

// watch follows a segment of the picture as it comes, until it has been
// fetched, and changes quality if the controller says to give it up.
func (s *stage) watch(l *leg, body *counted, length time.Duration, fetched <-chan struct{}) {
	var doubted time.Time // since when it has looked like one to give up
	for {
		select {
		case <-fetched:
			return
		case <-time.After(100 * time.Millisecond):
		}
		// A leg that waits for the one before it to end cannot be left for
		// another yet: the segment is judged once it can.
		s.mu.Lock()
		waits := l == s.pending
		s.mu.Unlock()
		if waits {
			continue
		}
		how := body.coming(time.Now())
		how.length = length
		if !doubted.IsZero() {
			how.doubted = time.Since(doubted)
		}
		to, why, doubt, ok := s.ctl.progress(l.quality, how)
		if ok {
			s.change(l, to, why)
			return
		}
		if !doubt {
			doubted = time.Time{}
		} else if doubted.IsZero() {
			doubted = time.Now()
		}
	}
}

// key answers for a key or an init section that a playlist names.
func (s *stage) key(w http.ResponseWriter, req *http.Request) {
	n, err := strconv.Atoi(req.PathValue("key"))
	s.mu.Lock()
	known := err == nil && n >= 0 && n < len(s.keys)
	var address *url.URL
	if known {
		address = s.keys[n]
	}
	s.mu.Unlock()
	if !known {
		http.NotFound(w, req)
		return
	}
	resp, err := ask(req.Context(), s.relay.direct, address.String(), req.Header.Get("Range"))
	if err != nil {
		refuse(w, req, file(address), err)
		return
	}
	defer resp.Body.Close()
	passHeaders(w, resp)
	if _, err := s.relay.pass(w, resp); err != nil {
		panic(http.ErrAbortHandler)
	}
}
