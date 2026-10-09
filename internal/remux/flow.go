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
	flow  *flow
	own   *flow     // how fast it alone comes, for a segment of the picture that is watched
	asked time.Time // when it was asked for
	of    int64     // how much of it there is, in bytes: its Content-Length

	mu    sync.Mutex
	got   int64
	first int64
	began time.Time // when the first of it came; zero before
	base  int64     // how much the flow had counted then
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

// coming tells how the body is coming, as the controller is told: how much
// of how much has come, not counting what came at once with the first of it,
// and how fast the whole stream and the body alone have been coming of late.
// Until a half second of it has been timed in full, each speed is that of
// what came after the first of it, over the time since. A half second timed
// in full in which nothing came is a stall: no speed at all.
func (c *counted) coming(now time.Time) coming {
	c.mu.Lock()
	defer c.mu.Unlock()
	how := coming{of: c.of, took: now.Sub(c.asked)}
	if c.began.IsZero() {
		return how
	}
	// The half second just past, and the tenth that is not over yet.
	from := now.Add(-(window + 1) * tenth)
	if from.Before(c.began) {
		from = c.began
	}
	how.got, how.of, how.flow = c.got-c.first, c.of-c.first, now.Sub(c.began)
	var sampled bool
	if how.speed, sampled = c.flow.fastest(from, now); !sampled {
		how.speed = transferRate(c.flow.bytes()-c.base, how.flow)
	}
	if how.rate, sampled = c.own.fastest(from, now); !sampled {
		how.rate = transferRate(c.got-c.first, how.flow)
	}
	return how
}
