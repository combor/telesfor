package remux

import (
	"io"
	"sync"
	"time"
)

// meter is a stream on its way to the viewer. It notes how much of the stream
// has gone, by the stream's own clock, and since when.
type meter struct {
	w io.Writer

	mu    sync.Mutex
	void  bool      // what comes goes nowhere: the stream is starting over
	began time.Time // when the first of it went; zero until then
	clock int64     // the furthest the stream's clock has got; -1 before the first
	span  int64     // how far it has run since the first, in its ticks
}

func newMeter(w io.Writer) *meter { return &meter{w: w, clock: -1} }

func (m *meter) Write(p []byte) (int, error) {
	m.mu.Lock()
	if m.void {
		m.mu.Unlock()
		return len(p), nil
	}
	if m.began.IsZero() {
		m.began = time.Now()
	}
	// What the splicer writes is whole packets.
	for packets := p; len(p)%packetSize == 0 && len(packets) > 0; packets = packets[packetSize:] {
		clock, ok := clockReference(packets[:packetSize])
		if !ok || packets[0] != 0x47 {
			continue
		}
		// A clock that steps back a little has to pass where it was before
		// more of the stream has gone. One that jumps has started anew, and
		// tells nothing of how long that took.
		const wrap = 1<<timestampBits - 1
		switch step := (clock - m.clock) & wrap; {
		case m.clock < 0:
		case step < 10*longFrame:
			m.span += step
		case (m.clock-clock)&wrap < 10*longFrame:
			continue
		}
		m.clock = clock
	}
	m.mu.Unlock()
	return m.w.Write(p)
}

// startOver tells whether the stream can start over, which it can while none
// of it has gone to the viewer. None goes then until it has: see started.
func (m *meter) startOver() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.void = m.began.IsZero()
	return m.void
}

// started is told that what comes from here on is of the stream as it was
// started over, if it was.
func (m *meter) started() {
	m.mu.Lock()
	m.void = false
	m.mu.Unlock()
}

// sent tells how much of the stream has gone to the viewer and how long ago
// the first of it went, or that none has.
func (m *meter) sent() (media, since time.Duration, onAir bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.began.IsZero() {
		return 0, 0, false
	}
	return time.Duration(m.span) * time.Second / longFrame, time.Since(m.began), true
}
