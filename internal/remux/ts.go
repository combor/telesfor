package remux

import "iter"

const (
	packetSize = 188 // bytes in an MPEG-TS packet

	// timestampBits is the size of an MPEG-TS timestamp. The clock wraps around
	// when they run out, a good day after it started.
	timestampBits = 33

	// wrap keeps a time, or how far apart two are, to timestampBits: the
	// clock wraps around.
	wrap = 1<<timestampBits - 1

	// clockRate is how many ticks a second the clock and the timestamps of
	// MPEG-TS count.
	clockRate = 90000
)

// packetsIn yields b packet by packet. What is left at its end that is too
// short to be a packet is left out.
func packetsIn(b []byte) iter.Seq[[]byte] {
	return func(yield func([]byte) bool) {
		for ; len(b) >= packetSize; b = b[packetSize:] {
			if !yield(b[:packetSize]) {
				return
			}
		}
	}
}

// pidOf returns the PID of a packet: which stream or table it is part of.
func pidOf(packet []byte) int {
	return int(packet[1]&0x1f)<<8 | int(packet[2])
}

// unitStart reports whether a table or a frame begins in a packet.
func unitStart(packet []byte) bool {
	return packet[1]&0x40 != 0
}

// payload returns what a packet carries after its header and adaptation
// field, or nil if it carries nothing.
func payload(packet []byte) []byte {
	start := 4
	if packet[3]&0x20 != 0 { // an adaptation field comes first
		start += 1 + int(packet[4])
	}
	if packet[3]&0x10 == 0 || start >= len(packet) {
		return nil
	}
	return packet[start:]
}

// randomAccess reports whether a packet is marked as a point to start decoding
// from, which is how ffmpeg marks keyframes.
func randomAccess(packet []byte) bool {
	hasAdaptationField := packet[3]&0x20 != 0
	return hasAdaptationField && packet[4] > 0 && packet[5]&0x40 != 0
}

// clockReference reads the time on the clock a packet carries, if it carries
// one, in the 90 kHz ticks that MPEG-TS timestamps count.
func clockReference(packet []byte) (int64, bool) {
	if packet[3]&0x20 == 0 || packet[4] < 7 || packet[5]&0x10 == 0 {
		return 0, false
	}
	return int64(packet[6])<<25 | int64(packet[7])<<17 | int64(packet[8])<<9 | int64(packet[9])<<1 | int64(packet[10])>>7, true
}

// setClockReference writes a time over the one on the clock a packet carries.
func setClockReference(packet []byte, t int64) {
	packet[6], packet[7], packet[8], packet[9] = byte(t>>25), byte(t>>17), byte(t>>9), byte(t>>1)
	packet[10] = packet[10]&0x7f | byte(t)<<7
}

// stamps returns the bytes that the timestamps of a frame are stored in, if
// one begins in the packet: its PTS, and its DTS if it has one of its own. A
// frame without is decoded when it is shown.
func stamps(packet []byte) (pts, dts []byte) {
	if packet[0] != 0x47 || !unitStart(packet) {
		return nil, nil // not a packet, or not one that a frame begins in
	}
	// A frame begins with a header: 00 00 01, the stream's id, a length, two
	// bytes of flags, the length of what follows, the PTS and the DTS.
	h := payload(packet)
	if len(h) < 14 || h[0] != 0 || h[1] != 0 || h[2] != 1 || h[6]&0xc0 != 0x80 || h[7]&0x80 == 0 {
		return nil, nil // no such header, or one without timestamps
	}
	if h[7]&0x40 == 0 || len(h) < 19 {
		return h[9:14], nil
	}
	return h[9:14], h[14:19]
}

// timestamp reads a timestamp from the five bytes it is spread over.
func timestamp(b []byte) int64 {
	return int64(b[0]>>1&7)<<30 | int64(b[1])<<22 | int64(b[2]>>1)<<15 | int64(b[3])<<7 | int64(b[4]>>1)
}

// setTimestamp writes a timestamp over the one in b. The bits between the
// parts of a timestamp stay as they are.
func setTimestamp(b []byte, t int64) {
	b[0] = b[0]&0xf1 | byte(t>>30&7)<<1
	b[1] = byte(t >> 22)
	b[2] = byte(t>>15)<<1 | 1
	b[3] = byte(t >> 7)
	b[4] = byte(t)<<1 | 1
}

// section returns the table section that begins in a packet, or nil if there
// is none.
func section(packet []byte) []byte {
	p := payload(packet)
	if p == nil {
		return nil
	}
	pointer := 1 + int(p[0]) // the section begins after a pointer to it
	if pointer >= len(p) {
		return nil
	}
	return p[pointer:]
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
