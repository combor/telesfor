package remux

import "sync/atomic"

const (
	// maxLate is how late a decoding time may be and still be taken for a
	// packager's mistake rather than for garbage: a second of the 90 kHz clock
	// that MPEG-TS timestamps count.
	maxLate = 90000

	// timestampBits is the size of an MPEG-TS timestamp. The clock wraps around
	// when they run out, a good day after it started.
	timestampBits = 33
)

// repairDTS makes sure that no frame in a run of MPEG-TS packets is to be
// decoded after it is shown. It leaves anything else as it is.
//
// Every frame carries two timestamps: when to decode it (DTS) and when to show
// it (PTS). Frames are stored in the order they are decoded in, which is not
// always the order they are shown in, so the first has to run ahead of the
// second. Some packagers leave that lead out, and stamp frames to be decoded
// after they were due on screen. TVP's does, on a handful of channels. ffmpeg
// then replaces the impossible times with guesses, frame by frame, and writes
// a stream whose frames are decoded in fits and starts.
//
// Moving all decoding times back by as much as the worst of them is late puts
// them in order and keeps them evenly spaced. All of a stream is late by the
// same amount, so late holds the most that any frame of it was late so far,
// and is kept from one call to the next. Only the frames that come before the
// first late one of a stream are passed on unrepaired.
func repairDTS(packets []byte, late *atomic.Int64) {
	timestamps(packets, func(pts, dts []byte) {
		// How long after the frame is shown it is decoded. The clock wraps
		// around, so a frame that is decoded in time comes out as one that is
		// nearly a whole turn of the clock late, which is out of the question.
		by := (timestamp(dts) - timestamp(pts)) & (1<<timestampBits - 1)
		move := late.Load()
		for ; by > move && by < maxLate; move = late.Load() {
			late.CompareAndSwap(move, by) // unless another segment of the stream got there first
		}
		if move > 0 {
			setTimestamp(dts, timestamp(dts)-move)
		}
	})
}

// timestamps calls fn for every frame in a run of MPEG-TS packets that carries
// both a PTS and a DTS, with the five bytes that each of them is stored in.
func timestamps(packets []byte, fn func(pts, dts []byte)) {
	for ; len(packets) >= packetSize; packets = packets[packetSize:] {
		if pts, dts := stamps(packets[:packetSize]); dts != nil {
			fn(pts, dts)
		}
	}
}

// stamps returns the bytes that the timestamps of a frame are stored in, if
// one begins in the packet: its PTS, and its DTS if it has one of its own. A
// frame without is decoded when it is shown.
func stamps(packet []byte) (pts, dts []byte) {
	if packet[0] != 0x47 || packet[1]&0x40 == 0 || packet[3]&0x10 == 0 {
		return nil, nil // not a packet, or not one that a frame begins in
	}
	header := 4
	if packet[3]&0x20 != 0 { // an adaptation field comes first
		header += 1 + int(packet[4])
	}
	if header+14 > packetSize {
		return nil, nil
	}
	// A frame begins with a header: 00 00 01, the stream's id, a length, two
	// bytes of flags, the length of what follows, the PTS and the DTS.
	h := packet[header:]
	if h[0] != 0 || h[1] != 0 || h[2] != 1 || h[6]&0xc0 != 0x80 || h[7]&0x80 == 0 {
		return nil, nil // no such header, or one without timestamps
	}
	if h[7]&0x40 == 0 || header+19 > packetSize {
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
