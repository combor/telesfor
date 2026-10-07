package remux

import (
	"context"
	"errors"
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

const (
	// maxSegment is the most of a segment to hold for ffmpeg. A larger one
	// is passed on as it comes, and fetched no faster than ffmpeg takes it.
	maxSegment = 16 << 20

	// maxPlaylist is the most of a playlist to read: France Télévisions'
	// hold four hours, at under a megabyte.
	maxPlaylist = 8 << 20
)

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
	gauge  *meter           // the stream on its way to the viewer
	start  func(*leg) error // starts the ffmpeg that reads a leg
	server *http.Server
	flow   flow // how fast audio and video have been coming

	mu      sync.Mutex
	legs    []*leg         // by their numbers, from 1; nil once nothing is to come of one
	current *leg           // the one whose ffmpeg is being listened to
	pending *leg           // the one to follow it, once a change is planned
	over    bool           // the stream has ended, and a leg found nothing left of it
	closed  bool           // the stage has stopped: no leg is to follow
	keys    []*url.URL     // the keys and init sections of the playlists, by the numbers ffmpeg asks for them with
	numbers map[string]int // those numbers, by address
}

// leg is a stretch of a stream in one quality, read by one ffmpeg.
type leg struct {
	n       int  // its number, from 1
	quality int  // which of the ladder's
	before  *leg // the leg it follows; nil for the first
	again   bool // it stands in for a leg that came to nothing
	anew    bool // it starts the stream over: nothing of it had reached the viewer

	video, sound track

	flying context.Context    // done once the segments on their way are given up
	drop   context.CancelFunc //
	stop   context.CancelFunc // stops its ffmpeg
	out    io.Reader          // what its ffmpeg writes
	wait   func() error       // waits for its ffmpeg to end, as often as it is asked
	failed error              // why its ffmpeg did not start
}

// track is the picture or the sound of a leg.
type track struct {
	list    playlist // as last fetched
	from    int64    // the first segment of the leg; -1 until its playlist has been read
	until   int64    // the segment the leg ends before; -1 while it goes on
	asked   int64    // the latest segment ffmpeg has asked for
	started int64    // the latest it has begun to get
}

// refusal is the status of an answer other than 200.
type refusal int

func (r refusal) Error() string { return strconv.Itoa(int(r)) + " " + http.StatusText(int(r)) }

// perform starts a stage for the stream that the relay is for.
func (r *relay) perform(l *ladder, ctl *controller, gauge *meter, start func(*leg) error) (*stage, error) {
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

// begin starts the first leg, in the given quality.
func (s *stage) begin(quality int) (*leg, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.current = s.add(quality, nil)
	return s.current, s.current.failed
}

// add makes the next leg and starts its ffmpeg. The caller holds the lock,
// so that nobody sees the leg before its ffmpeg is there to be read.
func (s *stage) add(quality int, before *leg) *leg {
	l := &leg{n: len(s.legs) + 1, quality: quality, before: before}
	l.flying, l.drop = context.WithCancel(context.Background())
	for _, t := range []*track{&l.video, &l.sound} {
		t.from, t.until, t.asked, t.started = -1, -1, -1, -1
	}
	s.legs = append(s.legs, l)
	l.failed = s.start(l)
	return l
}

// input is the address ffmpeg reads a leg from.
func (s *stage) input(l *leg) string {
	name := "video.m3u8"
	if s.ladder.sound != nil {
		name = "master.m3u8"
	}
	return fmt.Sprintf("http://%s/%d/%s", s.server.Addr, l.n, name)
}

// cut ends the leg with what ffmpeg has begun to get of it.
func (l *leg) cut() {
	l.video.until, l.sound.until = l.video.started+1, l.sound.started+1
	// ffmpeg asks for the sound of a segment after its picture, so the sound
	// may be a segment short of where the picture stops. The leg goes on to
	// there: the next then begins with both at the same time, as a stream
	// does, and as ffmpeg needs it to. See place.
	last, ok := l.video.list.find(l.video.until - 1)
	if !ok || l.sound.from < 0 {
		return
	}
	// By the clock, where the playlists tell the time. Those that do not are
	// taken to be cut alike: as many segments of the sound as of the picture.
	with, timed := l.sound.list.starting(last.at.Add(last.length))
	if !timed || last.at.IsZero() {
		with = l.sound.from + l.video.until - l.video.from
	}
	l.sound.until = max(l.sound.until, with)
}

// change plans the change to another quality: the leg ends with what ffmpeg
// has begun to get of it, and the next starts from there. The ffmpeg of the
// first stops by itself once it has read as much, and is stopped if it takes
// too long over it.
func (s *stage) change(l *leg, to int, why string) {
	s.mu.Lock()
	// A stream that has ended has nothing left to play in another quality.
	over := l.video.list.ended && l.video.started+1 >= l.video.list.next()
	if s.closed || l != s.current || s.pending != nil || to == l.quality || over {
		s.mu.Unlock()
		return
	}
	l.cut()
	patience := 3*l.video.list.target + 2*time.Second
	// A leg whose ffmpeg has had no picture to write has written nothing: it
	// is as if it had not been, and the next follows the one before it. And
	// as long as nothing has reached the viewer, the stream is started over:
	// its ffmpeg has had segments as they came, which cannot be taken back.
	// What that ffmpeg still writes goes nowhere then.
	anew := s.gauge.startOver()
	empty, before := anew || l.video.started < max(l.video.from, 0), l
	if empty {
		before = l.before
	}
	s.pending = s.add(to, before)
	s.pending.anew = anew
	s.mu.Unlock()

	s.ctl.changed(to, why)
	l.drop()
	if empty {
		l.stop()
	} else {
		time.AfterFunc(patience, l.stop)
	}
}

// after returns the leg that follows one whose ffmpeg has ended, or nil if
// the stream ends with it: because it is over, because an ffmpeg did not
// start, or for whatever the leg's own ffmpeg ended for. A leg that wrote
// nothing and has no other to follow it failed: the stream then goes on from
// where it was, in the quality it had. If it had none, it starts in the best,
// as it does when nothing is known of the connection.
func (s *stage) after(l *leg, wrote bool) (next *leg, over bool, err error) {
	best := len(s.ladder.qualities) - 1
	for {
		s.mu.Lock()
		next, over = s.pending, s.over
		if s.pending = nil; next == nil && !wrote && !over && !l.again && (l.before != nil || l.quality != best) {
			instead := best
			if l.before != nil {
				instead = l.before.quality
			}
			next = s.add(instead, l.before)
			next.again, next.anew = true, l.anew
		}
		if next != nil {
			s.current = next
			// A leg begins where the one before it ended, and stands in for
			// it. The legs before that are of no more use, and their
			// playlists may be hours long.
			for i, old := range s.legs {
				if old != next && old != next.before {
					s.legs[i] = nil
				}
			}
			if next.before != nil {
				next.before.before = nil
			}
		}
		s.mu.Unlock()
		if next == nil {
			return nil, over, err
		}
		if next.again {
			s.ctl.unfits(l.quality)
			s.ctl.changed(next.quality, "the other quality would not play")
		}
		if next.failed == nil {
			return next, false, nil
		}
		l, wrote, err = next, false, fmt.Errorf("remux: starting ffmpeg: %w", next.failed)
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
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
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
	t, address, letter := &l.sound, s.ladder.sound, "a"
	if video {
		t, address, letter = &l.video, s.ladder.qualities[l.quality].playlist, "v"
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
			s.refuse(w, req, file(address), err)
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
		placed := s.place(l, t, list, video)
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

	// ffmpeg asks for a segment while it still reads the one before, and
	// skips what it reads if the playlist no longer lists it.
	s.mu.Lock()
	from := max(t.from, t.asked-1)
	if t.until >= 0 {
		from = max(t.from, min(t.asked, t.until-1)-1)
	}
	text := t.list.write(from, t.until, max(t.from, 0), t.until >= 0 && t.list.next() >= t.until, func(kind string, seq int64, uri string) string {
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
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	io.WriteString(w, text)
}

// place finds where a leg begins in the playlist of its picture or sound, if
// that is not known yet. The first leg begins headStart segments before the
// newest. Every other begins where the leg before it ends, which is a
// segment of another playlist: it fails if the two cannot be lined up. The
// caller holds the lock.
//
// The picture and the sound of the first leg begin at the same time, as far
// as the playlists tell: whichever is placed second goes by the first. A
// segment may be listed between the two, and ffmpeg gives up on a picture
// that begins seconds after the sound.
func (s *stage) place(l *leg, t *track, list playlist, video bool) bool {
	if t.from >= 0 {
		return true
	}
	before, other := (*track)(nil), &l.video
	if video {
		other = &l.sound
	}
	if l.before != nil {
		if before = &l.before.sound; video {
			before = &l.before.video
		}
	}
	if before == nil || before.from < 0 {
		t.from = max(list.first(), list.next()-headStart)
		if first, placed := other.list.find(other.from); placed {
			if same, ok := list.starting(first.at); ok {
				t.from = max(list.first(), same)
			} else { // as many segments from the newest
				t.from = max(list.first(), list.next()-(other.list.next()-other.from))
			}
		}
		if video {
			s.relay.short.Store(len(list.segments) < headStart)
		}
	} else {
		ahead, ok := before.list.ahead(list)
		if !ok {
			return false
		}
		// What has left the playlist since cannot be played any more.
		t.from = max(before.until+ahead, list.first())
	}
	t.asked, t.started = t.from-1, t.from-1
	return true
}

// read fetches a playlist.
func (s *stage) read(ctx context.Context, address *url.URL) (playlist, error) {
	began := time.Now()
	fetch, err := http.NewRequestWithContext(ctx, http.MethodGet, address.String(), nil)
	if err != nil {
		return playlist{}, err
	}
	resp, err := s.relay.direct.Do(fetch)
	if err != nil {
		return playlist{}, errors.Unwrap(err) // without the URL, which may carry the stream's token
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

// refuse answers ffmpeg for what could not be fetched, the way upstream
// answered if it did.
func (s *stage) refuse(w http.ResponseWriter, req *http.Request, name string, err error) {
	status := http.StatusBadGateway
	if refused, ok := err.(refusal); ok {
		status = int(refused)
		slog.Warn("relay: upstream refused", "file", name, "status", refused.Error())
	} else if req.Context().Err() == nil { // a cancelled request only means that ffmpeg has gone
		slog.Warn("relay: upstream request failed", "file", name, "err", err)
	}
	http.Error(w, "upstream request failed", status)
}

// get asks upstream for a file that ffmpeg asks for.
func (s *stage) get(ctx context.Context, client *http.Client, req *http.Request, address *url.URL) (*http.Response, error) {
	fetch, err := http.NewRequestWithContext(ctx, http.MethodGet, address.String(), nil)
	if err != nil {
		return nil, err
	}
	if byteRange := req.Header.Get("Range"); byteRange != "" {
		fetch.Header.Set("Range", byteRange)
	}
	resp, err := client.Do(fetch)
	if err != nil {
		return nil, errors.Unwrap(err)
	}
	if resp.StatusCode >= http.StatusBadRequest {
		resp.Body.Close()
		return nil, refusal(resp.StatusCode)
	}
	return resp, nil
}

const (
	// atOnce is how soon after the first of a body what is read of it counts
	// as having come with the first: it was there already, and only read
	// later.
	atOnce = 20 * time.Millisecond

	// tenth is what the stream's flow is counted by, and window how many of
	// them tell a speed: half a second.
	tenth  = time.Second / 10
	window = 5
)

// flow counts audio and video together: concurrent segment fetches share
// the connection, so one alone tells too little of its speed.
type flow struct {
	mu     sync.Mutex
	since  time.Time // when the first of the tenths began
	tenths []int64
	total  int64 // all timed bytes, including those aged out of tenths
}

// add notes that n bytes have come.
func (f *flow) add(n int, now time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.total += int64(n)
	if f.since.IsZero() {
		f.since = now
	}
	// A read can be delayed before it acquires this shared lock.
	at := max(int(now.Sub(f.since)/tenth), 0)
	if at >= 6000 { // five minutes are more than any segment takes
		gone := at - 3000
		f.tenths, f.since, at = f.tenths[min(gone, len(f.tenths)):], f.since.Add(time.Duration(gone)*tenth), 3000
	}
	f.tenths = append(f.tenths, make([]int64, max(at+1-len(f.tenths), 0))...)
	f.tenths[at] += int64(n)
}

func (f *flow) bytes() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.total
}

// fastest returns the fastest complete half second between two moments, in
// bits per second. The boolean distinguishes no complete sample from a stall.
// The current, incomplete tenth is excluded; trailing empty tenths count.
func (f *flow) fastest(from, to time.Time) (float64, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.since.IsZero() {
		return 0, false
	}
	first := max(int(from.Sub(f.since)/tenth), 0)
	if f.since.Add(time.Duration(first) * tenth).Before(from) {
		first++ // exclude a bucket only partly inside the requested interval
	}
	last := int(to.Sub(f.since) / tenth)
	var came, most int64
	for i := first; i < last; i++ {
		if i < len(f.tenths) {
			came += f.tenths[i]
		}
		if i >= first+window && i-window < len(f.tenths) {
			came -= f.tenths[i-window]
		}
		if i >= first+window-1 {
			most = max(most, came)
		}
	}
	return transferRate(most, window*tenth), last-first >= window
}

// counted counts what is read of a body: how much has come, and how much of
// that at once with the first of it. What comes after goes to the flow.
type counted struct {
	io.ReadCloser
	flow *flow
	own  *flow // watched video only, for its remaining-time estimate

	mu    sync.Mutex
	got   int64
	first int64
	began time.Time // when the first of it came; zero before
	base  int64     // shared flow counter when this body began
}

func (c *counted) Read(p []byte) (int, error) {
	n, err := c.ReadCloser.Read(p)
	if n > 0 {
		now := time.Now()
		c.mu.Lock()
		if c.got += int64(n); c.began.IsZero() {
			c.began, c.first = now, int64(n)
			c.base = c.flow.bytes()
		} else if now.Sub(c.began) < atOnce {
			c.first += int64(n)
		} else {
			c.flow.add(n, now)
			if c.own != nil {
				c.own.add(n, now)
			}
		}
		c.mu.Unlock()
	}
	return n, err
}

// coming tells how the body is coming: how much has come, how much of that
// at once, for how long it has been coming, and the recent rates of the whole
// stream and this body. Before a complete sample exists, use bytes received
// since the first read; a complete sample of zero still means a stall.
func (c *counted) coming(now time.Time) (got, first int64, flowing time.Duration, speed, rate float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.began.IsZero() {
		return 0, 0, 0, 0, 0
	}
	// The half second just past, and the tenth that is not over yet.
	from := now.Add(-(window + 1) * tenth)
	if from.Before(c.began) {
		from = c.began
	}
	flowing = now.Sub(c.began)
	var sampled bool
	if speed, sampled = c.flow.fastest(from, now); !sampled {
		speed = transferRate(c.flow.bytes()-c.base, flowing)
	}
	if rate, sampled = c.own.fastest(from, now); !sampled {
		rate = transferRate(c.got-c.first, flowing)
	}
	return c.got, c.first, flowing, speed, rate
}

// spool takes in a body as fast as it comes, whatever ffmpeg does with it,
// and passes it on from the start, at once or once all of it has come.
type spool struct {
	mu     sync.Mutex
	more   *sync.Cond
	held   []byte // what has come and is still to be passed on
	closed bool   // no more is to come
	err    error  // why not, if not for its end
}

func newSpool() *spool {
	s := &spool{}
	s.more = sync.NewCond(&s.mu)
	return s
}

func (s *spool) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// A body of no end is taken in no faster than it is passed on.
	for len(s.held) >= maxSegment && !s.closed {
		s.more.Wait()
	}
	if s.closed {
		return 0, io.ErrClosedPipe
	}
	s.held = append(s.held, p...)
	s.more.Broadcast()
	return len(p), nil
}

// close marks the end of what comes: of all of it, or for the given reason.
func (s *spool) close(err error) {
	s.mu.Lock()
	if !s.closed {
		s.closed, s.err = true, err
	}
	s.more.Broadcast()
	s.mu.Unlock()
}

// wait returns once all has come, with the reason if it did not.
func (s *spool) wait() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for !s.closed {
		s.more.Wait()
	}
	return s.err
}

// passOn writes what has come and what is still to come to w. It returns the
// reason if not all of it came, or if w took none.
func (s *spool) passOn(w io.Writer) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for {
		for len(s.held) == 0 && !s.closed {
			s.more.Wait()
		}
		if len(s.held) == 0 {
			return s.err
		}
		piece := s.held
		s.held = nil
		s.mu.Unlock()
		_, err := w.Write(piece)
		s.mu.Lock()
		s.more.Broadcast()
		if err != nil {
			return err
		}
	}
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
	t := &l.sound
	if video {
		t = &l.video
	}
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
	// begin notes that ffmpeg is to get the segment now, unless the leg has
	// ended before it meanwhile.
	begin := func() bool {
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
	resp, err := s.get(ctx, s.relay.segments, req, wanted.uri)
	if err != nil {
		if whole && l.flying.Err() != nil {
			http.NotFound(w, req) // given up
			return
		}
		s.refuse(w, req, file(wanted.uri), err)
		return
	}
	body := &counted{ReadCloser: resp.Body, flow: &s.flow}
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
		go s.watch(l, body, resp.ContentLength, asked, wanted.length, fetched)
	}

	if whole = whole && resp.ContentLength > 0 && resp.ContentLength <= maxSegment; whole {
		if err := spool.wait(); err != nil || !begin() {
			if err != nil && l.flying.Err() == nil && req.Context().Err() == nil {
				slog.Warn("relay: upstream transfer failed", "file", file(wanted.uri), "err", err)
			}
			http.NotFound(w, req) // nothing of it has reached ffmpeg, which goes on to the next
			return
		}
	} else if !keep() || !begin() { // given up, or the leg ends before it
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
func (s *stage) watch(l *leg, body *counted, of int64, asked time.Time, length time.Duration, fetched <-chan struct{}) {
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
		got, first, flowing, speed, rate := body.coming(time.Now())
		how := coming{got: got - first, of: of - first, speed: speed, rate: rate, flow: flowing, took: time.Since(asked), length: length}
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
	resp, err := s.get(req.Context(), s.relay.direct, req, address)
	if err != nil {
		s.refuse(w, req, file(address), err)
		return
	}
	defer resp.Body.Close()
	passHeaders(w, resp)
	if _, err := s.relay.pass(w, resp); err != nil {
		panic(http.ErrAbortHandler)
	}
}

// passHeaders starts the answer to ffmpeg the way upstream answered.
func passHeaders(w http.ResponseWriter, resp *http.Response) {
	for _, name := range []string{"Content-Type", "Content-Range", "Accept-Ranges"} {
		if value := resp.Header.Get(name); value != "" {
			w.Header().Set(name, value)
		}
	}
	w.WriteHeader(resp.StatusCode)
}
