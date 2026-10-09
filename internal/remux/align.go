package remux

import (
	"bytes"
	"io"
	"log/slog"
)

// maxLeadIn is how much of a stream may pass before all of its streams have
// started. After that it is passed on as it is.
const maxLeadIn = 12 << 20

// aligner passes MPEG-TS on from a point where Plex can start reading, and
// drops what comes before.
//
// A live stream whose audio and video are fetched separately may begin with
// seconds of one and nothing of the other, and even one that begins with both
// begins with video. Players cope, but Plex gives up on a channel when it
// finds no audio where it starts reading, and its remux for a player crashes
// when a stream's first audio frame comes after its first video frame. So the
// output begins at the first keyframe that every other stream has started
// before, with what those streams were sending in front of it.
type aligner struct {
	w       io.Writer
	limit   int  // maxLeadIn, unless a test says otherwise
	started bool // everything is passed on from here

	partial  []byte         // an incomplete packet, kept for the next Write
	pending  []byte         // the packets since the last keyframe: the output, if the aligner gives up
	pat, pmt []byte         // the latest packets of the tables that describe the streams
	pmtPID   int            // -1 until the PAT has been read
	clockPID int            // the stream that carries the clock: the video, if there is any
	others   []int          // the other streams, in the order of the PMT
	inFlight map[int][]byte // what each of the others has sent since its latest frame began
	waiting  map[int]bool   // streams that have not started; nil until the PMT has been read
	seen     int            // bytes read so far
	dropped  int            // bytes dropped so far
}

func newAligner(w io.Writer) *aligner {
	return &aligner{w: w, limit: maxLeadIn, pmtPID: -1, clockPID: -1}
}

func (a *aligner) Write(p []byte) (int, error) {
	if a.started {
		return a.w.Write(p)
	}

	a.partial = append(a.partial, p...)
	for len(a.partial) >= packetSize && !a.started {
		a.started = a.read(a.partial[:packetSize])
		a.partial = a.partial[packetSize:]
	}
	if !a.started {
		return len(p), nil
	}

	slog.Debug("remux: stream starts", "dropped", a.dropped)
	start := append(a.pending, a.partial...)
	a.partial, a.pending, a.pat, a.pmt, a.inFlight = nil, nil, nil, nil, nil
	_, err := a.w.Write(start)
	return len(p), err
}

// read takes in one packet and reports whether the output can start with it.
func (a *aligner) read(packet []byte) (start bool) {
	if packet[0] != 0x47 || a.seen >= a.limit {
		// Not MPEG-TS, or a stream that is listed but does not show up.
		slog.Warn("remux: cannot find where all streams have started; passing the stream on as it is")
		a.pending = append(a.pending, packet...)
		return true
	}
	a.seen += packetSize

	pid := pidOf(packet)
	begins := unitStart(packet) // a table or a frame begins in this packet
	switch {
	case pid == 0 && begins:
		a.pat = bytes.Clone(packet)
		a.pmtPID = programMapPID(section(packet))
	case pid == a.pmtPID && begins:
		a.pmt = bytes.Clone(packet)
		clock, streams := programMap(section(packet))
		if a.waiting == nil {
			a.clockPID, a.waiting, a.inFlight = clock, map[int]bool{}, map[int][]byte{}
			for _, stream := range streams {
				a.waiting[stream] = true
				if stream != clock {
					a.others = append(a.others, stream)
					a.inFlight[stream] = nil
				}
			}
		}
	}
	if begins && a.waiting != nil {
		delete(a.waiting, pid)
	}

	if begins && pid == a.clockPID && randomAccess(packet) {
		// A keyframe: a better place to start from than the one before. The
		// tables go in front of it, so that the output opens with them.
		a.dropped += len(a.pending)
		a.pending = append(append(a.pending[:0], a.pat...), a.pmt...)
		if a.waiting != nil && len(a.waiting) == 0 {
			// Every other stream has started, so this is where the output
			// begins. What they were in the middle of goes first.
			for _, other := range a.others {
				a.pending = append(a.pending, a.inFlight[other]...)
			}
			a.pending = append(a.pending, packet...)
			return true
		}
	}
	a.pending = append(a.pending, packet...)

	if sent, ok := a.inFlight[pid]; ok && (begins || len(sent) > 0) {
		if begins {
			sent = sent[:0]
		}
		a.inFlight[pid] = append(sent, packet...)
	}
	return false
}
