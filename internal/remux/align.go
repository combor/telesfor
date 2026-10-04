package remux

import (
	"bytes"
	"io"
	"log/slog"
)

const (
	packetSize = 188 // bytes in an MPEG-TS packet

	// maxLeadIn is how much of a stream may pass before all of its streams have
	// started. After that it is passed on as it is.
	maxLeadIn = 12 << 20
)

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

	pid := int(packet[1]&0x1f)<<8 | int(packet[2])
	begins := packet[1]&0x40 != 0 // a table or a frame begins in this packet
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

// randomAccess reports whether a packet is marked as a point to start decoding
// from, which is how ffmpeg marks keyframes.
func randomAccess(packet []byte) bool {
	hasAdaptationField := packet[3]&0x20 != 0
	return hasAdaptationField && packet[4] > 0 && packet[5]&0x40 != 0
}

// section returns the table section that begins in a packet, or nil if there
// is none.
func section(packet []byte) []byte {
	payload := 4
	if packet[3]&0x20 != 0 { // an adaptation field comes first
		payload += 1 + int(packet[4])
	}
	if packet[3]&0x10 == 0 || payload >= packetSize {
		return nil
	}
	pointer := payload + 1 + int(packet[payload]) // the section begins after a pointer to it
	if pointer >= packetSize {
		return nil
	}
	return packet[pointer:]
}

// sectionEnd returns where the entries of a table section end: before the
// checksum that closes it.
func sectionEnd(s []byte) int {
	length := int(s[1]&0x0f)<<8 | int(s[2]) // counted from the byte after the length
	return min(3+length-4, len(s))
}

// programMapPID reads a program association table and returns the PID of the
// program map table, or -1.
func programMapPID(s []byte) int {
	if len(s) < 8 {
		return -1
	}
	for i := 8; i+4 <= sectionEnd(s); i += 4 {
		if program := int(s[i])<<8 | int(s[i+1]); program != 0 { // 0 is the network table
			return int(s[i+2]&0x1f)<<8 | int(s[i+3])
		}
	}
	return -1
}

// programMap reads a program map table and returns the PID of the stream that
// carries the clock, and the PIDs of all streams.
func programMap(s []byte) (clock int, streams []int) {
	if len(s) < 12 {
		return -1, nil
	}
	clock = int(s[8]&0x1f)<<8 | int(s[9])
	programInfo := int(s[10]&0x0f)<<8 | int(s[11])
	for i := 12 + programInfo; i+5 <= sectionEnd(s); {
		streams = append(streams, int(s[i+1]&0x1f)<<8|int(s[i+2]))
		streamInfo := int(s[i+3]&0x0f)<<8 | int(s[i+4])
		i += 5 + streamInfo
	}
	return clock, streams
}
