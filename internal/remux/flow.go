package remux

import (
	"io"
	"sync"
	"time"
)

const (
	// atOnce is how soon after the first of a body what is read of it counts
	// as having come with the first: it was there already, and only read
	// later.
	atOnce = 20 * time.Millisecond

	// tenth is what the stream's flow is counted by, and window how many of
	// them tell a speed: half a second.
	tenth  = time.Second / 10
	window = 5
)

// flow counts audio and video together: concurrent segment fetches share
// the connection, so one alone tells too little of its speed.
type flow struct {
	mu     sync.Mutex
	since  time.Time // when the first of the tenths began
	tenths []int64
	total  int64 // all timed bytes, including those aged out of tenths
}

// add notes that n bytes have come.
func (f *flow) add(n int, now time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.total += int64(n)
	if f.since.IsZero() {
		f.since = now
	}
	// A read can be delayed before it acquires this shared lock.
	at := max(int(now.Sub(f.since)/tenth), 0)
	if at >= 6000 { // five minutes are more than any segment takes
		gone := at - 3000
		f.tenths, f.since, at = f.tenths[min(gone, len(f.tenths)):], f.since.Add(time.Duration(gone)*tenth), 3000
	}
	f.tenths = append(f.tenths, make([]int64, max(at+1-len(f.tenths), 0))...)
	f.tenths[at] += int64(n)
}

func (f *flow) bytes() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.total
}

// fastest returns the fastest complete half second between two moments, in
// bits per second. The boolean distinguishes no complete sample from a stall.
// The current, incomplete tenth is excluded; trailing empty tenths count.
func (f *flow) fastest(from, to time.Time) (float64, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.since.IsZero() {
		return 0, false
	}
	first := max(int(from.Sub(f.since)/tenth), 0)
	if f.since.Add(time.Duration(first) * tenth).Before(from) {
		first++ // exclude a bucket only partly inside the requested interval
	}
	last := int(to.Sub(f.since) / tenth)
	var came, most int64
	for i := first; i < last; i++ {
		if i < len(f.tenths) {
			came += f.tenths[i]
		}
		if i >= first+window && i-window < len(f.tenths) {
			came -= f.tenths[i-window]
		}
		if i >= first+window-1 {
			most = max(most, came)
		}
	}
	return transferRate(most, window*tenth), last-first >= window
}

// counted counts what is read of a body: how much has come, and how much of
// that at once with the first of it. What comes after goes to the flow.
type counted struct {
	io.ReadCloser
	flow *flow
	own  *flow // watched video only, for its remaining-time estimate

	mu    sync.Mutex
	got   int64
	first int64
	began time.Time // when the first of it came; zero before
	base  int64     // shared flow counter when this body began
}

func (c *counted) Read(p []byte) (int, error) {
	n, err := c.ReadCloser.Read(p)
	if n > 0 {
		now := time.Now()
		c.mu.Lock()
		if c.got += int64(n); c.began.IsZero() {
			c.began, c.first = now, int64(n)
			c.base = c.flow.bytes()
		} else if now.Sub(c.began) < atOnce {
			c.first += int64(n)
		} else {
			c.flow.add(n, now)
			if c.own != nil {
				c.own.add(n, now)
			}
		}
		c.mu.Unlock()
	}
	return n, err
}

// coming tells how the body is coming: how much has come, how much of that
// at once, for how long it has been coming, and the recent rates of the whole
// stream and this body. Before a complete sample exists, use bytes received
// since the first read; a complete sample of zero still means a stall.
func (c *counted) coming(now time.Time) (got, first int64, flowing time.Duration, speed, rate float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.began.IsZero() {
		return 0, 0, 0, 0, 0
	}
	// The half second just past, and the tenth that is not over yet.
	from := now.Add(-(window + 1) * tenth)
	if from.Before(c.began) {
		from = c.began
	}
	flowing = now.Sub(c.began)
	var sampled bool
	if speed, sampled = c.flow.fastest(from, now); !sampled {
		speed = transferRate(c.flow.bytes()-c.base, flowing)
	}
	if rate, sampled = c.own.fastest(from, now); !sampled {
		rate = transferRate(c.got-c.first, flowing)
	}
	return c.got, c.first, flowing, speed, rate
}
