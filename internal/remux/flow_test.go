package remux

import (
	"testing"
	"time"
)

// Parallel audio must not make the first video probe mistake its share of
// the connection for the whole connection's speed.
func TestProbeBeforeFiveCompleteBuckets(t *testing.T) {
	start := time.Unix(100, 0)
	for _, test := range []struct {
		first, probe time.Duration
	}{
		{100 * time.Millisecond, 501 * time.Millisecond},
		{120 * time.Millisecond, 600 * time.Millisecond},
	} {
		f := &flow{}
		for i := range 5 {
			at := start.Add(test.first + time.Duration(i)*tenth)
			f.add(25000, at) // picture
			f.add(25000, at) // sound
		}
		body := &counted{flow: f, own: &flow{}, began: start, first: 4096, got: 4096 + 125000}
		got, first, elapsed, speed, rate := body.coming(start.Add(test.probe))
		if speed < 3e6 {
			t.Errorf("at %v measured %.2f Mbps, losing the audio contribution", test.probe, speed/1e6)
		}
		r := &route{}
		c := newController("probe", franceLadder, r, func() (time.Duration, time.Duration, bool) { return 0, 0, false })
		to, _, _, change := c.progress(c.start(), coming{got: got - first, of: 5 << 20, flow: elapsed, took: elapsed, speed: speed, rate: rate, length: 7680 * time.Millisecond})
		if !change || c.ladder[to].height < 540 {
			t.Errorf("at %v selected quality %d, want at least 540p", test.probe, to)
		}
	}
}

// A segment's speed is that of the picture and the sound together, which
// share the connection, and a connection that has stalled has none.
func TestProbeCountsBothTracks(t *testing.T) {
	start := time.Unix(100, 0)
	f := &flow{}
	for i := range 8 {
		at := start.Add(time.Duration(i) * tenth)
		f.add(12000, at) // video
		f.add(6750, at)  // audio: together 1.5 Mbps
	}
	for _, elapsed := range []time.Duration{550 * time.Millisecond, 800 * time.Millisecond} {
		now := start.Add(elapsed)
		body := &counted{flow: f, own: &flow{}, began: start, first: 4096, got: 4096 + 64<<10}
		got, first, flowing, speed, rate := body.coming(now)
		if speed != 1.5e6 {
			t.Fatalf("at %v measured %.0f bps, want audio and video together at 1.5 Mbps", elapsed, speed)
		}
		r := &route{}
		c := newController("probe", franceLadder, r, func() (time.Duration, time.Duration, bool) { return 0, 0, false })
		quality := c.start()
		to, _, _, change := c.progress(quality, coming{got: got - first, of: 5 << 20, flow: flowing, took: elapsed, speed: speed, rate: rate, length: 7680 * time.Millisecond})
		if !change || c.ladder[to].height != 216 {
			t.Errorf("at %v selected quality %d (change %t), want 216p", elapsed, to, change)
		}
	}
	body := &counted{flow: f, own: &flow{}, began: start, first: 4096, got: 4096 + 64<<10}
	if _, _, _, speed, _ := body.coming(start.Add(2 * time.Second)); speed != 0 {
		t.Errorf("a stalled stream measured %.0f bps, want zero", speed)
	}
}

// A tenth of a second that began before a segment did is not counted in its
// speed.
func TestProbePartialFirstBucket(t *testing.T) {
	start := time.Unix(1000, 0)
	f := &flow{}
	f.add(1, start) // a previous segment established the bucket alignment
	began := start.Add(90 * time.Millisecond)
	for i := 1; i <= 5; i++ {
		f.add(50000, began.Add(time.Duration(i)*tenth))
	}
	body := &counted{flow: f, own: &flow{}, began: began, base: 1, first: 4096, got: 4096 + 250000}
	if _, _, _, speed, _ := body.coming(began.Add(500 * time.Millisecond)); speed != 4e6 {
		t.Fatalf("partially covered first bucket measured %.2f Mbps, want 4 Mbps", speed/1e6)
	}
}

// Timestamps can arrive out of order when another read gets the lock first.
func TestFlowOutOfOrder(t *testing.T) {
	start := time.Unix(1000, 0)
	for _, late := range []time.Duration{99 * time.Millisecond, 100 * time.Millisecond, 250 * time.Millisecond} {
		t.Run(late.String(), func(t *testing.T) {
			f := &flow{}
			f.add(100, start)
			f.add(50, start.Add(-late))
			if got, sampled := f.fastest(start, start.Add(500*time.Millisecond)); !sampled || got != 150*8/0.5 {
				t.Errorf("late sample: %.0f bps, sampled %t; want all 150 bytes counted", got, sampled)
			}
		})
	}
}

// The connection's speed is told by its fastest half second.
func TestFlowFastest(t *testing.T) {
	since := time.Unix(1000, 0)
	for _, test := range []struct {
		name   string
		tenths []int64 // the bytes that came in each tenth of a second
		want   float64 // in bits a second
	}{
		{"evenly", []int64{100, 100, 100, 100, 100, 100, 100}, 500 * 8 / 0.5},
		{"slowly at first, as on a connection that was idle", []int64{10, 20, 40, 80, 160, 320, 640, 640, 640, 640, 100}, 2880 * 8 / 0.5},
		{"in a burst after a wait", []int64{0, 0, 0, 0, 0, 0, 900, 100}, 1000 * 8 / 0.5},
		{"in less than half a second", []int64{500, 500, 500}, 0},
	} {
		f := &flow{}
		for i, n := range test.tenths {
			f.add(int(n), since.Add(time.Duration(i)*tenth))
		}
		if got, _ := f.fastest(since, since.Add(time.Duration(len(test.tenths))*tenth)); got != test.want {
			t.Errorf("%s: fastest() = %.0f, want %.0f", test.name, got, test.want)
		}
	}
	// Only what came between the two moments counts.
	f := &flow{}
	for i, n := range []int64{900, 900, 900, 900, 900, 100, 100, 100, 100, 100, 100} {
		f.add(int(n), since.Add(time.Duration(i)*tenth))
	}
	if got, _ := f.fastest(since.Add(500*time.Millisecond), since.Add(1100*time.Millisecond)); got != 500*8/0.5 {
		t.Errorf("after its first half second: fastest() = %.0f, want %.0f", got, 500*8/0.5)
	}
	// What is long past is forgotten, a stream that stood still for a while
	// included.
	later := since.Add(time.Hour)
	for i := range 6 {
		f.add(200, later.Add(time.Duration(i)*tenth))
	}
	if got, _ := f.fastest(later, later.Add(600*time.Millisecond)); got != 1000*8/0.5 || len(f.tenths) > 6000 {
		t.Errorf("an hour later: fastest() = %.0f with %d tenths of a second kept, want %.0f and no more than ten minutes", got, len(f.tenths), 1000*8/0.5)
	}
}
