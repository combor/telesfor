package remux

import (
	"bytes"
	"sync/atomic"
	"testing"
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

// segment builds a segment the way TVP's packager does: a keyframe without a
// DTS, and then frames that are stored in the order P B B and shown in the
// order B B P. The decoding times come late by the given amount: at 0, each
// B-frame is decoded just as it is shown.
func segment(start, late int64) []byte {
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

func TestTimestamp(t *testing.T) {
	for _, test := range []struct {
		time  int64
		bytes []byte
	}{
		{0, []byte{0x21, 0x00, 0x01, 0x00, 0x01}},
		{90000, []byte{0x21, 0x00, 0x05, 0xbf, 0x21}}, // a second
		{1<<33 - 1, []byte{0x2f, 0xff, 0xff, 0xff, 0xff}},
	} {
		if got := timestamp(test.bytes); got != test.time {
			t.Errorf("timestamp(% x) = %d, want %d", test.bytes, got, test.time)
		}
		for _, before := range [][]byte{{0x21, 0x00, 0x01, 0x00, 0x01}, {0x2f, 0xff, 0xff, 0xff, 0xff}} {
			got := bytes.Clone(before)
			setTimestamp(got, test.time)
			if !bytes.Equal(got, test.bytes) {
				t.Errorf("setTimestamp(% x, %d) left % x, want % x", before, test.time, got, test.bytes)
			}
		}
	}
}

func TestRepairDTS(t *testing.T) {
	// A start so late that the timestamps of the segment wrap around.
	const wrapping = 1<<33 - 3*frameLength/2

	for _, test := range []struct {
		name        string
		start, late int64
		moved       int64 // how far the decoding times should be moved back
	}{
		{"decoded as shown", 900000, 0, 0},
		{"decoded ahead of time", 900000, -frameLength, 0},
		{"decoded a frame late", 900000, frameLength, frameLength},
		{"decoded two frames late", 900000, 2 * frameLength, 2 * frameLength},
		{"decoded a frame late, as the clock wraps", wrapping, frameLength, frameLength},
		{"decoded as shown, as the clock wraps", wrapping, 0, 0},
		{"timestamps that make no sense", 900000, 60 * 90000, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			var late atomic.Int64
			first := segment(test.start, test.late)
			repairDTS(first, &late)
			if got := late.Load(); got != test.moved {
				t.Fatalf("found the stream to be %d late, want %d", got, test.moved)
			}

			// The first segment is repaired from its first late frame on:
			// the one frame before that keeps the decoding time it came with.
			const early = 5 // where that frame's packet is in the segment
			want := segment(test.start, test.late-test.moved)
			copy(want[early*packetSize:], segment(test.start, test.late)[early*packetSize:][:packetSize])
			if !bytes.Equal(first, want) {
				t.Error("the first segment is not the same segment with its frames decoded in time")
			}

			// Nothing but the decoding times may change, and from the second
			// segment on, all of them do.
			next := segment(test.start+10*frameLength, test.late)
			repairDTS(next, &late)
			if !bytes.Equal(next, segment(test.start+10*frameLength, test.late-test.moved)) {
				t.Error("the next segment is not the same segment with its frames decoded in time")
			}
		})
	}

	t.Run("not MPEG-TS", func(t *testing.T) {
		broken := segment(900000, frameLength)
		for name, file := range map[string][]byte{
			"empty":              nil,
			"out of step":        broken[1:],
			"without sync bytes": bytes.ReplaceAll(broken, []byte{0x47}, []byte{0x48}),
		} {
			var late atomic.Int64
			got := bytes.Clone(file)
			if repairDTS(got, &late); late.Load() != 0 || !bytes.Equal(got, file) {
				t.Errorf("%s: changed the file, want it left alone", name)
			}
		}
	})
}
