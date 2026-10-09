package remux

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"time"
)

// leg is a stretch of a stream in one quality, read by one ffmpeg.
type leg struct {
	n          int  // its number, from 1
	quality    int  // which of the ladder's
	before     *leg // the leg it follows; nil for the first
	standsIn   bool // it stands in for a leg that came to nothing
	startsOver bool // it starts the stream over: nothing of it had reached the viewer

	video, sound track

	flying context.Context    // done once the segments on their way are given up
	drop   context.CancelFunc //
	stop   context.CancelFunc // stops its ffmpeg
	out    io.Reader          // what its ffmpeg writes
	wait   func() error       // waits for its ffmpeg to end, as often as it is asked
	failed error              // why its ffmpeg did not start
}

// track is the picture or the sound of a leg. The stage's lock guards it.
type track struct {
	list    playlist // as last fetched
	from    int64    // the first segment of the leg; -1 until its playlist has been read
	until   int64    // the segment the leg ends before; -1 while it goes on
	asked   int64    // the latest segment ffmpeg has asked for
	started int64    // the latest it has begun to get
}

// track returns the picture of the leg, or its sound.
func (l *leg) track(video bool) *track {
	if video {
		return &l.video
	}
	return &l.sound
}

// placed tells whether it is known where in its playlist the leg begins.
func (t *track) placed() bool { return t.from >= 0 }

// within tells whether a segment comes before the end of the leg, if it has
// one.
func (t *track) within(seq int64) bool { return t.until < 0 || seq < t.until }

// complete tells whether the playlist lists all that the leg has: the leg
// ends, and the playlist gets as far.
func (t *track) complete() bool { return t.until >= 0 && t.list.next() >= t.until }

// listFrom is where the playlist that ffmpeg is given begins: at the segment
// before the latest of the leg's that it has asked for. ffmpeg asks for a
// segment while it still reads the one before, and skips what it reads if the
// playlist no longer lists it.
func (t *track) listFrom() int64 {
	last := t.asked
	if t.until >= 0 {
		last = min(t.asked, t.until-1)
	}
	return max(t.from, last-1)
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
	l.failed = s.start(l, s.input(l))
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
	if !ok || !l.sound.placed() {
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
	s.pending.startsOver = anew
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
// start, or for whatever the leg's own ffmpeg ended with, ended. Its error is
// why, and nil if the stream is over. A leg that wrote nothing and has no
// other to follow it failed: the stream then goes on from where it was, in
// the quality it had. If it had none, it starts in the best, as it does when
// nothing is known of the connection.
func (s *stage) after(l *leg, wrote bool, ended error) (*leg, error) {
	var failed error // why the ffmpeg of the leg to follow did not start
	for {
		s.mu.Lock()
		next, over := s.pending, s.over
		s.pending = nil
		if next == nil && !wrote && !over {
			next = s.standIn(l)
		}
		if next != nil {
			s.takeOver(next)
		}
		s.mu.Unlock()
		if next == nil {
			if over {
				return nil, nil
			}
			return nil, cmp.Or(failed, ended)
		}
		if next.standsIn {
			s.ctl.unfits(l.quality)
			s.ctl.changed(next.quality, "the other quality would not play")
		}
		if next.failed == nil {
			return next, nil
		}
		l, wrote, failed = next, false, fmt.Errorf("remux: starting ffmpeg: %w", next.failed)
	}
}

// standIn starts a leg in place of l, which wrote nothing and has none to
// follow it: in the quality of the leg before, from where that ended, or in
// the best if l was the first. It starts none for a leg that stands in itself,
// or where that is what failed. The caller holds the lock.
func (s *stage) standIn(l *leg) *leg {
	best := len(s.ladder.qualities) - 1
	if l.standsIn || (l.before == nil && l.quality == best) {
		return nil
	}
	instead := best
	if l.before != nil {
		instead = l.before.quality
	}
	next := s.add(instead, l.before)
	next.standsIn, next.startsOver = true, l.startsOver
	return next
}

// takeOver makes a leg the current one. The leg begins where the one before
// it ended, so the legs before that are of no more use, and their playlists
// may be hours long. The caller holds the lock.
func (s *stage) takeOver(next *leg) {
	s.current = next
	for i, old := range s.legs {
		if old != next && old != next.before {
			s.legs[i] = nil
		}
	}
	if next.before != nil {
		next.before.before = nil
	}
}

// place finds where a leg begins in the playlist of its picture or sound, if
// that is not known yet. The first leg joins the stream live: see joinLive.
// Every other begins where the leg before it ends, which is a segment of
// another playlist: it fails if the two cannot be lined up. The caller holds
// the lock.
func (s *stage) place(l *leg, list playlist, video bool) bool {
	t := l.track(video)
	if t.placed() {
		return true
	}
	if l.before == nil || !l.before.track(video).placed() {
		t.from = joinLive(list, l.track(!video))
		if video {
			s.relay.short.Store(len(list.segments) < headStart)
		}
	} else {
		before := l.before.track(video)
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

// joinLive is where the first leg begins in a playlist: headStart segments
// before the newest. Its picture and its sound begin at the same time, as far
// as the playlists tell: whichever is placed second goes by the first. A
// segment may be listed between the two, and ffmpeg gives up on a picture
// that begins seconds after the sound.
func joinLive(list playlist, other *track) int64 {
	from := list.next() - headStart
	if first, ok := other.list.find(other.from); ok {
		if same, timed := list.starting(first.at); timed {
			from = same
		} else { // as many segments from the newest
			from = list.next() - (other.list.next() - other.from)
		}
	}
	return max(list.first(), from)
}
