package remux

import (
	"bytes"
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
