package remux

import (
	"cmp"
	"io"
)

const (
	// maxJoin is how much of a stream may pass before its first frame shows.
	// After that it is joined on by its clock.
	maxJoin = 4 << 20

	// anyFrame is how long a frame is taken to last when the stream before had
	// no two frames to tell by: a twenty-fifth of a second.
	anyFrame = clockRate / 25

	// longFrame is longer than any frame lasts: a second.
	longFrame = clockRate
)

// splicer makes one MPEG-TS stream of what one ffmpeg after another writes.
//
// A stream changes quality by ending one ffmpeg and starting the next: see
// Copy. Each ffmpeg writes a stream of its own, with timestamps that begin at
// 1.4 seconds and continuity counters that begin at zero, and Plex is to see
// a single stream that goes on. So the timestamps of every stream after the
// first are moved by what makes its first video frame be shown a frame after
// the last of the stream before. The sound moves by as much, which keeps it
// where the source has it against the picture. And every PID's counter counts
// on from where it was.
//
// It is when frames are shown that the streams share, not when they are
// decoded: a quality without frames that are stored out of order, as the
// lowest often is, decodes each as it is shown, where another decodes two
// frames ahead. Where the next stream would have a frame decoded before the
// last of the one before, that frame is decoded just after it instead.
type splicer struct {
	w       io.Writer
	partial []byte // an incomplete packet, kept for the next Write
	out     []byte // what a Write passes on, kept to be used again

	pmtPID   int // -1 until the PAT has been read
	clockPID int // the stream the others are timed by: the video, if there is any

	counters map[int]byte // the continuity counter each PID has got to
	turned   map[int]byte // how far the counters of the stream at hand are turned on, by PID
	shift    int64        // how far its timestamps are moved on
	joining  bool         // its first frame has not shown yet
	held     []byte       // its packets so far, while joining

	decoded int64 // when the latest frame of the clock's stream is decoded; -1 before the first
	shown   int64 // when the last of its frames so far is shown; -1 before the first
	frame   int64 // how long a frame lasts: what two in a row were decoded apart, twice over
	step    int64 // what the latest two frames were decoded apart
	clock   int64 // the latest time on the clock; -1 before the first
	floor   int64 // where the clock of the stream before stopped, until this one's has passed it; -1 after
}

func newSplicer(w io.Writer) *splicer {
	return &splicer{w: w, pmtPID: -1, clockPID: -1, counters: map[int]byte{}, turned: map[int]byte{}, decoded: -1, shown: -1, clock: -1, floor: -1}
}

// next tells the splicer that the stream at hand has ended: what is written
// from here on is another, to be joined on to it.
func (s *splicer) next() {
	s.partial, s.turned, s.shift, s.floor = nil, map[int]byte{}, 0, s.clock
	s.joining = s.decoded >= 0 // a stream that follows nothing stays as it is
}

func (s *splicer) Write(p []byte) (int, error) {
	s.partial = append(s.partial, p...)
	s.out = s.out[:0]
	for len(s.partial) >= packetSize {
		s.read(s.partial[:packetSize])
		s.partial = s.partial[packetSize:]
	}
	if len(s.out) == 0 {
		return len(p), nil
	}
	_, err := s.w.Write(s.out)
	return len(p), err
}

// read takes in one packet of the stream at hand.
func (s *splicer) read(packet []byte) {
	if packet[0] != 0x47 { // not MPEG-TS: there is nothing to join
		s.out = append(s.out, packet...)
		return
	}
	pid := pidOf(packet)
	if begins := unitStart(packet); begins && pid == 0 {
		s.pmtPID = programMapPID(section(packet))
	} else if begins && pid == s.pmtPID {
		s.clockPID, _ = programMap(section(packet))
	}
	if !s.joining {
		s.out = append(s.out, s.join(packet)...)
		return
	}

	// The stream is held until it tells when its first frame is shown: the
	// first that is stored is the first to be shown, as a stream begins with
	// a keyframe.
	s.held = append(s.held, packet...)
	if pts, _ := stamps(packet); pid == s.clockPID && pts != nil {
		s.shift = s.shown + cmp.Or(s.frame, anyFrame) - timestamp(pts)
	} else if len(s.held) < maxJoin {
		return
	} else if s.clock >= 0 {
		// A stream without frames where they should be: its clock goes on
		// from where the other stopped.
		for held := range packetsIn(s.held) {
			if first, ok := clockReference(held); ok {
				s.shift = s.clock - first
				break
			}
		}
	}
	s.joining = false
	for held := range packetsIn(s.held) {
		s.out = append(s.out, s.join(held)...)
	}
	s.held = nil
}

// join makes a packet of the stream at hand one of the stream that goes out,
// and returns it.
func (s *splicer) join(packet []byte) []byte {
	pid := pidOf(packet)
	s.moveClock(packet)
	if pts, dts := stamps(packet); pts != nil && pid != 0 && pid != s.pmtPID { // tables have no frames in them
		s.moveFrame(pts, dts)
		if pid == s.clockPID {
			s.follow(pts, dts)
		}
	}
	s.countOn(packet, pid)
	return packet
}

// moveClock moves the clock a packet carries, if it carries one, and notes
// where it got to.
func (s *splicer) moveClock(packet []byte) {
	clock, ok := clockReference(packet)
	if !ok {
		return
	}
	if s.shift != 0 || s.floor >= 0 {
		clock = (clock + s.shift) & wrap
		// A stream whose sound begins before its picture starts its clock
		// that much earlier. The clock of the stream that goes out does not
		// step back: it waits.
		if back := (s.floor - clock) & wrap; s.floor >= 0 && back < longFrame {
			clock = s.floor
		} else {
			s.floor = -1
		}
		setClockReference(packet, clock)
	}
	s.clock = clock
}

// moveFrame moves when a frame is decoded and shown, as far as the stream at
// hand is moved.
func (s *splicer) moveFrame(pts, dts []byte) {
	if s.shift == 0 {
		return
	}
	setTimestamp(pts, (timestamp(pts)+s.shift)&wrap)
	if dts != nil {
		setTimestamp(dts, (timestamp(dts)+s.shift)&wrap)
	}
}

// follow notes when a frame of the clock's stream is decoded and shown, and
// how long frames last. It keeps the frame from being decoded before the one
// before it.
func (s *splicer) follow(pts, dts []byte) {
	shown := timestamp(pts)
	decoded := shown
	if dts != nil {
		decoded = timestamp(dts)
		// Not before the frame before it, which a stream that decodes
		// further ahead than the one before would have.
		if back := (s.decoded - decoded) & wrap; s.decoded >= 0 && back < longFrame {
			decoded = (s.decoded + 1) & wrap
			setTimestamp(dts, decoded)
		}
	}
	// The clock wraps around, and a step that makes no sense tells nothing.
	// Nor does one that is not like the one before: frames that were moved
	// are a tick apart.
	if step := (decoded - s.decoded) & wrap; s.decoded >= 0 && step > 0 && step < longFrame {
		if step == s.step || s.frame == 0 {
			s.frame = step
		}
		s.step = step
	}
	s.decoded = decoded
	if s.shown < 0 || (shown-s.shown)&wrap < 1<<(timestampBits-1) {
		s.shown = shown
	}
}

// countOn makes a packet's continuity counter count on from where its PID's
// had got to. A PID's counter counts the packets that carry something.
func (s *splicer) countOn(packet []byte, pid int) {
	turn, turning := s.turned[pid]
	if !turning {
		if last, ok := s.counters[pid]; ok {
			turn = (last + packet[3]>>4&1 - packet[3]) & 0x0f
		}
		s.turned[pid] = turn
	}
	counter := (packet[3] + turn) & 0x0f
	packet[3] = packet[3]&0xf0 | counter
	s.counters[pid] = counter
}
