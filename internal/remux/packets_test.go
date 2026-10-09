package remux

import (
	"bytes"
	"testing"
)

// The PIDs of the stream the tests build.
const (
	tablePID = 0x1000 // the program map table
	videoPID = 0x100
	audioPID = 0x101
)

// packet builds an MPEG-TS packet. A keyframe is marked the way ffmpeg marks
// one, as a random access point. The payload tells packets apart.
func packet(pid int, begins, keyframe bool, payload ...byte) []byte {
	p := []byte{0x47, byte(pid >> 8), byte(pid), 0x10}
	if begins {
		p[1] |= 0x40
	}
	if keyframe {
		p[3] |= 0x20
		p = append(p, 1, 0x40) // an adaptation field of one byte: its flags
	}
	p = append(p, payload...)
	for len(p) < packetSize {
		p = append(p, 0xff)
	}
	return p
}

// programMapPacket builds the table that lists the streams, with the clock on
// the video.
func programMapPacket(streams ...int) []byte {
	section := []byte{
		0x02, 0, 0, // table id; the section's length, filled in below
		0, 1, 0xc1, 0, 0, // program number, version, section numbers
		0xe0 | videoPID>>8, videoPID & 0xff, // the stream that carries the clock
		0xf0, 0, // no descriptors of the program
	}
	for _, pid := range streams {
		section = append(section, 0x1b, 0xe0|byte(pid>>8), byte(pid), 0xf0, 0) // type, PID, no descriptors
	}
	section = append(section, 0, 0, 0, 0) // a checksum that nobody checks
	section[1], section[2] = 0xb0|byte((len(section)-3)>>8), byte(len(section)-3)
	return packet(tablePID, true, false, append([]byte{0}, section...)...)
}

// The packets the test streams are made of.
var (
	pat = packet(0, true, false,
		0,                // the section begins right here
		0x00, 0xb0, 0x0d, // table id; the section's length
		0, 1, 0xc1, 0, 0, // transport stream id, version, section numbers
		0, 1, 0xe0|tablePID>>8, tablePID&0xff, // program 1 is described at tablePID
		0, 0, 0, 0, // a checksum that nobody checks
	)
	pmt   = programMapPacket(videoPID, audioPID)
	key1  = packet(videoPID, true, true, 'k', 1) // a keyframe begins
	key2  = packet(videoPID, true, true, 'k', 2)
	frame = packet(videoPID, true, false, 'f')  // another frame begins
	more  = packet(videoPID, false, false, 'm') // more of a frame
	aud1  = packet(audioPID, true, false, 'a', 1)
	aud2  = packet(audioPID, true, false, 'a', 2)
	rest  = packet(audioPID, false, false, 'r') // more of an audio frame
)

// frameLength is how long a frame lasts at 50 frames a second, on the clock
// that MPEG-TS timestamps count.
const frameLength = 1800

// frameStart builds the packet that a video frame begins in. Like the real
// ones, it carries the stream's clock in an adaptation field ahead of the
// frame. A negative dts leaves the DTS out of the frame's header.
func frameStart(pts, dts int64) []byte {
	p := []byte{0x47, 0x40 | videoPID>>8, videoPID & 0xff, 0x30,
		7, 0x10, 0, 0, 0, 0, 0, 0} // an adaptation field of seven bytes: its flags and a clock
	header := []byte{0, 0, 1, 0xe0, 0, 0, 0x80, 0x80, 5, 0x21, 0, 1, 0, 1}
	setTimestamp(header[9:14], pts)
	if dts >= 0 {
		header[7], header[8] = 0xc0, 10
		header[9] |= 0x10 // a PTS that a DTS follows is marked differently
		header = append(header, 0x11, 0, 1, 0, 1)
		setTimestamp(header[14:19], dts)
	}
	p = append(append(p, header...), 'f')
	for len(p) < packetSize {
		p = append(p, 0xff)
	}
	return p
}

// tvpSegment builds a segment the way TVP's packager does: a keyframe
// without a DTS, and then frames that are stored in the order P B B and
// shown in the order B B P. The decoding times come late by the given
// amount: at 0, each B-frame is decoded just as it is shown.
func tvpSegment(start, late int64) []byte {
	s := bytes.Join([][]byte{pat, pmt, frameStart(start, -1), more, aud1}, nil)
	for i := range int64(3) {
		shown := start + (1+3*i)*frameLength // when the first B-frame of the three is shown
		decoded := shown - frameLength + late
		s = append(s, frameStart(shown+2*frameLength, decoded)...)
		s = append(s, more...)
		s = append(s, frameStart(shown, decoded+frameLength)...)
		s = append(s, frameStart(shown+frameLength, decoded+2*frameLength)...)
		s = append(s, aud2...)
	}
	return s
}

// checkContinuity checks that the continuity counter of every PID counts on
// from one packet with a payload to the next, as MPEG-TS has it, and reports
// the first that does not.
func checkContinuity(t *testing.T, stream []byte) {
	t.Helper()
	counters := map[int]byte{}
	for packet := range packetsIn(stream) {
		pid, counter := pidOf(packet), packet[3]&0x0f
		if last, ok := counters[pid]; ok && packet[3]&0x10 != 0 && counter != (last+1)&0x0f {
			t.Errorf("PID %#x: a packet is counted %d after one counted %d", pid, counter, last)
			return
		}
		counters[pid] = counter
	}
}
