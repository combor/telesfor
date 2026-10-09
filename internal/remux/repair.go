package remux

import "sync/atomic"

// maxLate is how late a decoding time may be and still be taken for a
// packager's mistake rather than for garbage: a second.
const maxLate = clockRate

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
	for packet := range packetsIn(packets) {
		pts, dts := stamps(packet)
		if dts == nil {
			continue
		}
		// How long after the frame is shown it is decoded. The clock wraps
		// around, so a frame that is decoded in time comes out as one that is
		// nearly a whole turn of the clock late, which is out of the question.
		by := (timestamp(dts) - timestamp(pts)) & wrap
		move := late.Load()
		for ; by > move && by < maxLate; move = late.Load() {
			late.CompareAndSwap(move, by) // unless another segment of the stream got there first
		}
		if move > 0 {
			setTimestamp(dts, timestamp(dts)-move)
		}
	}
}
