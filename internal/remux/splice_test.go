package remux

import (
	"bytes"
	"slices"
	"testing"
)

// written builds a stream the way an ffmpeg writes one: the tables, and then
// frames of a length that are decoded from start on, each with the clock in
// it, shown so much later than it is decoded, and with the sound that plays
// as it is shown. The continuity counters count from zero.
func written(start int64, frames int, length, ahead int64) []byte {
	const wrap = 1<<timestampBits - 1
	packets := [][]byte{pat, pmt}
	for i := range int64(frames) {
		decoded := (start + i*length) & wrap
		video := frameStart((decoded+ahead)&wrap, decoded)
		setClockReference(video, decoded-63000)
		sound := frameStart((decoded+ahead)&wrap, -1)
		sound[1], sound[2], sound[5] = 0x40|audioPID>>8, audioPID&0xff, 0 // no clock in the sound
		packets = append(packets, video, more, sound)
	}
	counters := map[int]byte{}
	var stream []byte
	for _, packet := range packets {
		pid := int(packet[1]&0x1f)<<8 | int(packet[2])
		stream = append(stream, packet...)
		stream[len(stream)-packetSize+3] |= counters[pid]
		counters[pid] = (counters[pid] + 1) & 0x0f
	}
	return stream
}

// timing is what a stream says of time: when its frames are decoded and
// shown, when its sound plays and what its clock shows, in the order they
// come.
type timing struct{ decoded, shown, sound, clock []int64 }

func timingOf(t *testing.T, stream []byte) timing {
	t.Helper()
	var all timing
	counters := map[int]byte{}
	for ; len(stream) >= packetSize; stream = stream[packetSize:] {
		packet := stream[:packetSize]
		pid := int(packet[1]&0x1f)<<8 | int(packet[2])
		if last, ok := counters[pid]; ok && packet[3]&0x0f != (last+1)&0x0f {
			t.Errorf("PID %#x: a packet is counted %d after one counted %d", pid, packet[3]&0x0f, last)
		}
		counters[pid] = packet[3] & 0x0f
		if clock, ok := clockReference(packet); ok {
			all.clock = append(all.clock, clock)
		}
		switch pts, dts := stamps(packet); {
		case pid == videoPID && dts != nil:
			all.decoded, all.shown = append(all.decoded, timestamp(dts)), append(all.shown, timestamp(pts))
		case pid == audioPID && pts != nil:
			all.sound = append(all.sound, timestamp(pts))
		}
	}
	return all
}

func TestSplicer(t *testing.T) {
	// Each ffmpeg starts at 1.4 seconds, whatever it reads. The second stream
	// has frames twice as long as the first, and the third joins on to that.
	// All show a frame one frame of the first after it is decoded.
	first, slower, third := written(126000, 5, frameLength, frameLength), written(126000, 4, 2*frameLength, frameLength), written(126000, 3, frameLength, frameLength)

	var whole bytes.Buffer
	s := newSplicer(&whole)
	s.Write(bytes.Clone(first))
	if !bytes.Equal(whole.Bytes(), first) {
		t.Fatal("the first stream was changed, want it passed on as it is")
	}
	s.next()
	s.Write(bytes.Clone(slower))
	s.next()
	s.Write(bytes.Clone(third))

	got := timingOf(t, whole.Bytes())
	var decoded []int64
	for i := range int64(5) {
		decoded = append(decoded, 126000+i*frameLength)
	}
	for i := range int64(4) { // a frame after the last of the first stream
		decoded = append(decoded, 126000+5*frameLength+i*2*frameLength)
	}
	for i := range int64(3) { // and a frame of the second after its last
		decoded = append(decoded, 126000+5*frameLength+4*2*frameLength+i*frameLength)
	}
	if !slices.Equal(got.decoded, decoded) {
		t.Errorf("frames are decoded at\n%v, want one stream going on:\n%v", got.decoded, decoded)
	}
	// The sound plays when its frame is shown, and the clock runs 0.7 seconds
	// behind the decoding: both must have moved with the picture.
	if !slices.Equal(got.sound, got.shown) || got.shown[0] != decoded[0]+frameLength {
		t.Errorf("the sound plays at\n%v, want it to keep its place against the picture, shown at\n%v", got.sound, got.shown)
	}
	for i, clock := range got.clock {
		if clock != decoded[i]-63000 {
			t.Errorf("the clock shows %d at frame %d, want it %d behind the frame", clock, i, 63000)
		}
	}
	if len(whole.Bytes()) != len(first)+len(slower)+len(third) {
		t.Errorf("%d bytes came out, want the %d that went in", whole.Len(), len(first)+len(slower)+len(third))
	}

	// ffmpeg's writes do not end where packets end.
	var pieces bytes.Buffer
	s = newSplicer(&pieces)
	for _, stream := range [][]byte{first, slower, third} {
		for piece := range slices.Chunk(bytes.Clone(stream), 100) {
			s.Write(piece)
		}
		s.next()
	}
	if !bytes.Equal(pieces.Bytes(), whole.Bytes()) {
		t.Error("written in pieces, the streams came out differently")
	}

	// Timestamps wrap around, a good day into a stream.
	t.Run("as the clock wraps", func(t *testing.T) {
		var out bytes.Buffer
		s := newSplicer(&out)
		s.Write(written(1<<timestampBits-3*frameLength, 2, frameLength, frameLength)) // the stream before ends a frame short of the turn
		s.next()
		s.Write(written(126000, 3, frameLength, frameLength))
		want := []int64{1<<timestampBits - 3*frameLength, 1<<timestampBits - 2*frameLength, 1<<timestampBits - frameLength, 0, frameLength}
		if got := timingOf(t, out.Bytes()); !slices.Equal(got.decoded, want) {
			t.Errorf("frames are decoded at %v, want %v", got.decoded, want)
		}
	})

	// Streams share when their frames are shown, not when they are decoded:
	// a quality whose frames are stored in the order they are shown in
	// decodes each as it is shown, and another two frames ahead.
	t.Run("streams that decode differently far ahead", func(t *testing.T) {
		const start, f = 126000, frameLength
		ahead, level := written(start, 3, f, 2*f), written(start, 3, f, 0)

		var out bytes.Buffer
		s := newSplicer(&out)
		s.Write(bytes.Clone(ahead))
		s.next()
		s.Write(bytes.Clone(level))
		got := timingOf(t, out.Bytes())
		shown := []int64{start + 2*f, start + 3*f, start + 4*f, start + 5*f, start + 6*f, start + 7*f}
		if !slices.Equal(got.shown, shown) || !slices.Equal(got.sound, shown) || !slices.Equal(got.decoded[3:], shown[3:]) {
			t.Errorf("frames are shown at %v with their sound at %v and decoded at %v, want them shown at %v, and the last three decoded then too", got.shown, got.sound, got.decoded, shown)
		}

		// The other way round, the first frames of the second stream would
		// be decoded before the last of the first: they come just after it.
		out.Reset()
		s = newSplicer(&out)
		s.Write(bytes.Clone(level))
		s.next()
		s.Write(bytes.Clone(ahead))
		got = timingOf(t, out.Bytes())
		shown = []int64{start, start + f, start + 2*f, start + 3*f, start + 4*f, start + 5*f}
		decoded := []int64{start, start + f, start + 2*f, start + 2*f + 1, start + 2*f + 2, start + 3*f}
		if !slices.Equal(got.shown, shown) || !slices.Equal(got.sound, shown) || !slices.Equal(got.decoded, decoded) {
			t.Errorf("frames are shown at %v with their sound at %v and decoded at %v, want them shown at %v and decoded at %v", got.shown, got.sound, got.decoded, shown, decoded)
		}
	})

	// The sound of a stream may begin before its picture, and its clock
	// with it: before where the clock of the stream before stopped.
	t.Run("a clock that begins earlier", func(t *testing.T) {
		early := written(126000, 3, frameLength, frameLength)
		for packets := early; len(packets) >= packetSize; packets = packets[packetSize:] {
			if clock, ok := clockReference(packets[:packetSize]); ok {
				setClockReference(packets[:packetSize], clock-2*frameLength)
			}
		}
		var out bytes.Buffer
		s := newSplicer(&out)
		s.Write(bytes.Clone(first))
		s.next()
		s.Write(early)
		got := timingOf(t, out.Bytes())
		last := got.clock[len(first)/packetSize/3-1] // what the clock of the first stream got to
		for i, clock := range got.clock[1:] {
			if clock < got.clock[i] {
				t.Errorf("the clock steps back from %d to %d", got.clock[i], clock)
			}
		}
		if want := []int64{last, last, last + frameLength}; !slices.Equal(got.clock[len(got.clock)-3:], want) {
			t.Errorf("the clock of the second stream shows %v, want it to wait and then go on: %v", got.clock[len(got.clock)-3:], want)
		}
	})

	// An ffmpeg that was stopped before it wrote anything leaves nothing to
	// join on to.
	t.Run("after nothing", func(t *testing.T) {
		var out bytes.Buffer
		s := newSplicer(&out)
		s.next()
		s.Write(bytes.Clone(first))
		if !bytes.Equal(out.Bytes(), first) {
			t.Error("the stream was changed, want it passed on as it is")
		}
	})

	// A stream whose frames never show is joined on by its clock, and passed
	// on all the same.
	t.Run("without frames", func(t *testing.T) {
		clock := frameStart(0, -1)
		clock[1], clock[2] = 0x1f, 0xfe // a PID that the tables do not list
		setClockReference(clock, 5*90000)
		odd := bytes.Repeat(clock, maxJoin/packetSize+1)

		var out bytes.Buffer
		s := newSplicer(&out)
		s.Write(bytes.Clone(first))
		s.next()
		s.Write(odd)
		joined := out.Bytes()[len(first):]
		if len(joined) != len(odd) {
			t.Fatalf("%d bytes of it came out, want all %d", len(joined), len(odd))
		}
		last := 126000 + 4*frameLength - 63000 // what the clock of the first stream got to
		if got, _ := clockReference(joined[:packetSize]); got != int64(last) {
			t.Errorf("its clock starts at %d, want it to go on from %d", got, last)
		}
	})
}
