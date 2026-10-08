package remux

import (
	"bytes"
	"sync/atomic"
	"testing"
)

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
			first := tvpSegment(test.start, test.late)
			repairDTS(first, &late)
			if got := late.Load(); got != test.moved {
				t.Fatalf("found the stream to be %d late, want %d", got, test.moved)
			}

			// The first segment is repaired from its first late frame on:
			// the one frame before that keeps the decoding time it came with.
			const early = 5 // where that frame's packet is in the segment
			want := tvpSegment(test.start, test.late-test.moved)
			copy(want[early*packetSize:], tvpSegment(test.start, test.late)[early*packetSize:][:packetSize])
			if !bytes.Equal(first, want) {
				t.Error("the first segment is not the same segment with its frames decoded in time")
			}

			// Nothing but the decoding times may change, and from the second
			// segment on, all of them do.
			next := tvpSegment(test.start+10*frameLength, test.late)
			repairDTS(next, &late)
			if !bytes.Equal(next, tvpSegment(test.start+10*frameLength, test.late-test.moved)) {
				t.Error("the next segment is not the same segment with its frames decoded in time")
			}
		})
	}

	t.Run("not MPEG-TS", func(t *testing.T) {
		broken := tvpSegment(900000, frameLength)
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
