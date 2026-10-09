package remux

import (
	"fmt"
	"io"
	"log/slog"
	"math"
	"strings"
	"testing"
	"time"
)

// The ladders of two providers, as their master playlists had them on
// 2026-10-06: TVP's three qualities in segments of two seconds, whose top
// takes half as much again at times as on average, and France Télévisions'
// six in segments of nearly eight.
var (
	tvpLadder = &ladder{qualities: []quality{
		{rate: 1047200, average: 761200, height: 288},
		{rate: 4056800, average: 2741200, height: 576},
		{rate: 10243200, average: 6811200, height: 1080},
	}}
	franceLadder = &ladder{qualities: []quality{
		{rate: 303314, average: 235347, height: 144},
		{rate: 611736, average: 545600, height: 216},
		{rate: 1213146, average: 1150547, height: 360},
		{rate: 1727145, average: 1645600, height: 540},
		{rate: 3017784, average: 2855564, height: 720},
		{rate: 6068449, average: 5715564, height: 1080},
	}}
)

const (
	simStep    = 10 * time.Millisecond
	simDelay   = 60 * time.Millisecond   // before the first byte of a segment comes
	simRestart = 500 * time.Millisecond  // for another ffmpeg to take over
	simHold    = 5500 * time.Millisecond // what Plex was measured to hold back, a little more than the controller takes it to
	simStartup = 2 * time.Second         // what a player wants in hand before it starts
)

// viewer is a stream in a simulated world: what stage and ffmpeg do with it,
// and the player that Plex shows it in.
type viewer struct {
	ctl     *controller // nil for a viewer of the best quality, whatever happens
	joins   time.Duration
	quality int

	next      int           // the segment to fetch
	fetching  bool          // one is on its way
	asked     time.Duration // when it was asked for
	size, got float64       // in bits
	fastest   float64       // the most bits a second it has come at
	doubted   time.Duration // since when it has looked like one to give up; -1 if it does not
	ready     time.Duration // when the ffmpeg that takes over can read

	began     time.Duration // when the first segment was given; -1 before
	delivered time.Duration // how much of the stream has been given
	played    time.Duration
	playing   bool
	frozen    bool

	freezes int
	froze   time.Duration
	changes int
	top     time.Duration // how long the best quality played
	reached time.Duration // when it first played; -1 if never
	lowest  int
}

// world is a connection, a provider's live stream and its viewers, in time
// that is counted rather than waited for.
type world struct {
	ladder  *ladder
	segment time.Duration                  // how long a segment plays
	speed   func(at time.Duration) float64 // of the connection, in bits a second
	delay   func(seq int) time.Duration    // before a segment's first byte, on top of simDelay
	at      time.Duration
	start   time.Time
	route   *route
	viewers []*viewer
	note    func(format string, args ...any) // is told of every change of quality
}

// size is how many bits a segment of a quality has: its average, more or
// less, up to the quality's peak.
func (w *world) size(quality, seq int) float64 {
	q := w.ladder.qualities[quality]
	swing := float64(q.rate)/float64(q.average) - 1
	_, part := math.Modf(float64(seq) * 0.618034)
	return float64(q.average) * w.segment.Seconds() * (1 + swing*(2*part-1))
}

// listed tells whether the provider lists a segment yet. Six are there to
// begin with, as the head start asks.
func (w *world) listed(seq int) bool {
	return time.Duration(seq-headStart+1)*w.segment <= w.at
}

func (w *world) join(adapts bool, at time.Duration) *viewer {
	v := &viewer{joins: at, began: -1, reached: -1, quality: len(w.ladder.qualities) - 1}
	if adapts {
		v.ctl = newController("test", w.ladder, w.route, func() (time.Duration, time.Duration, bool) {
			return v.delivered, w.at - v.began, v.began >= 0
		})
		v.ctl.now = func() time.Time { return w.start.Add(w.at) }
		v.ctl.log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	w.viewers = append(w.viewers, v)
	return v
}

// change is what stage does when the controller says to change quality.
func (w *world) change(v *viewer, to int, why string) {
	if w.note != nil {
		w.note("%v: %s to %s: %s", w.at.Round(100*time.Millisecond), w.ladder.qualities[v.quality], w.ladder.qualities[to], why)
	}
	v.ctl.changed(to, why)
	v.quality, v.changes, v.ready = to, v.changes+1, w.at+simRestart
	v.lowest = min(v.lowest, to)
}

func (w *world) run(length time.Duration) {
	for ; w.at < length; w.at += simStep {
		// The connection is shared by those whose segments are coming.
		fetching := 0
		for _, v := range w.viewers {
			if v.fetching && w.at >= v.asked+simDelay+w.delay(v.next) {
				fetching++
			}
		}
		for _, v := range w.viewers {
			if w.at < v.joins {
				continue
			}
			if w.at == v.joins {
				if v.ctl != nil {
					v.quality = v.ctl.start()
				}
				v.lowest = v.quality
			}
			w.fetch(v, fetching)
			w.play(v)
		}
	}
}

// fetch moves the fetching of a viewer's stream on by a step. The connection
// is shared by as many viewers as are fetching.
func (w *world) fetch(v *viewer, fetching int) {
	if !v.fetching {
		if w.at >= v.ready && w.listed(v.next) {
			v.fetching, v.asked, v.got, v.fastest, v.size, v.doubted = true, w.at, 0, 0, w.size(v.quality, v.next), -1
		}
		return
	}
	first := v.asked + simDelay + w.delay(v.next)
	if w.at >= first {
		v.got += w.speed(w.at) / float64(fetching) * simStep.Seconds()
		if w.at-first >= 500*time.Millisecond { // it has been coming for long enough to tell
			v.fastest = max(v.fastest, w.speed(w.at)/float64(fetching))
		}
	}
	flow, took := max(w.at-first, 0), w.at-v.asked
	if v.got < v.size {
		// stage watches a segment that it holds whole: any but of the lowest quality.
		if v.ctl != nil && v.quality > 0 && took%(100*time.Millisecond) == 0 {
			how := coming{got: int64(v.got / 8), of: int64(v.size / 8), speed: w.speed(w.at) / float64(fetching), rate: w.speed(w.at) / float64(fetching), flow: flow, took: took, length: w.segment}
			if v.doubted >= 0 {
				how.doubted = w.at - v.doubted
			}
			to, why, doubt, ok := v.ctl.progress(v.quality, how)
			if ok {
				v.fetching = false
				w.change(v, to, why)
			}
			if !doubt {
				v.doubted = -1
			} else if v.doubted < 0 {
				v.doubted = w.at
			}
		}
		return
	}
	if v.ctl != nil {
		v.ctl.gives(w.segment)
	}
	v.fetching, v.next, v.delivered = false, v.next+1, v.delivered+w.segment
	if v.began < 0 {
		v.began = w.at
	}
	if v.ctl != nil {
		how := arrival{size: int64(v.size / 8), wait: first - v.asked, flow: flow, fastest: v.fastest, length: w.segment, newest: !w.listed(v.next)}
		if to, why, ok := v.ctl.fetched(v.quality, how); ok {
			w.change(v, to, why)
		}
	}
}

// play moves a viewer's player on by a step. It plays what has come but for
// what Plex holds back, and freezes when it has nothing. Like any player, it
// starts once it has a little to go on.
func (w *world) play(v *viewer) {
	playable := v.delivered - simHold
	if !v.playing {
		v.playing = v.began >= 0 && playable >= simStartup
		return
	}
	if v.played >= playable {
		if !v.frozen {
			v.freezes++
		}
		v.frozen, v.froze = true, v.froze+simStep
		return
	}
	v.frozen, v.played = false, v.played+simStep
	if v.quality == len(w.ladder.qualities)-1 {
		if v.top += simStep; v.reached < 0 {
			v.reached = w.at
		}
	}
}

// TestAdaptation plays streams over connections that are too slow, or turn
// slow, or change all the time, with the controller and without, and compares
// how often and how long the picture freezes. Run with -v, it prints what
// came of each.
func TestAdaptation(t *testing.T) {
	const mbps = 1e6
	steady := func(speed float64) func(time.Duration) float64 {
		return func(time.Duration) float64 { return speed }
	}
	type outcome struct {
		freezes, changes int
		froze, reached   time.Duration
	}
	for _, test := range []struct {
		name    string
		speed   func(at time.Duration) float64
		delay   func(seq int) time.Duration
		viewers []time.Duration // when each joins; one at the start if none
		check   func(t *testing.T, adapting, best outcome, l *ladder)
	}{
		{
			name:  "a connection that is fast enough",
			speed: steady(50 * mbps),
			check: func(t *testing.T, a, _ outcome, l *ladder) {
				if a.freezes > 0 || a.changes > 0 || a.reached > 15*time.Second {
					t.Errorf("froze %d times and changed quality %d times, with the best first played after %v: want the best from the start", a.freezes, a.changes, a.reached)
				}
			},
		},
		{
			name:  "a connection that is slow from the start",
			speed: steady(2 * mbps),
			check: func(t *testing.T, a, best outcome, l *ladder) {
				if a.freezes > 1 || a.froze > time.Second || best.freezes < 10 {
					t.Errorf("froze %d times for %v, against %d times in the best quality: want hardly a freeze where the best quality has many", a.freezes, a.froze, best.freezes)
				}
			},
		},
		{
			name: "a connection that turns slow at once",
			speed: func(at time.Duration) float64 {
				if at < time.Minute {
					return 30 * mbps
				}
				return 3 * mbps
			},
			check: func(t *testing.T, a, best outcome, l *ladder) {
				if a.freezes > 0 || best.freezes == 0 || a.changes > 2 {
					t.Errorf("froze %d times, against %d in the best quality, and changed quality %d times: want a step down and no freeze", a.freezes, best.freezes, a.changes)
				}
			},
		},
		{
			name: "a connection that turns slow little by little",
			speed: func(at time.Duration) float64 {
				return (30 - 29*min(max(at-time.Minute, 0).Minutes()/5, 1)) * mbps
			},
			check: func(t *testing.T, a, best outcome, l *ladder) {
				if a.freezes > 0 || best.freezes == 0 || a.changes >= 2*len(l.qualities) {
					t.Errorf("froze %d times, against %d in the best quality, and changed quality %d times: want it to step down as it goes, and no freeze", a.freezes, best.freezes, a.changes)
				}
			},
		},
		{
			name:  "a segment that is slow to start",
			speed: steady(30 * mbps),
			delay: func(seq int) time.Duration {
				if seq == 40 {
					return 3 * time.Second
				}
				return 0
			},
			check: func(t *testing.T, a, _ outcome, l *ladder) {
				if a.changes > 0 || a.freezes > 0 {
					t.Errorf("changed quality %d times and froze %d times: want one slow segment to change nothing", a.changes, a.freezes)
				}
			},
		},
		{
			name: "a connection that recovers",
			speed: func(at time.Duration) float64 {
				if at < 2*time.Minute {
					return 3 * mbps
				}
				return 30 * mbps
			},
			check: func(t *testing.T, a, best outcome, l *ladder) {
				if a.freezes > 1 || a.froze > time.Second || a.reached < 0 || a.reached > 2*time.Minute+time.Duration(len(l.qualities))*90*time.Second {
					t.Errorf("froze %d times for %v, and first played the best quality after %v: want hardly a freeze, and the best quality a while after two minutes", a.freezes, a.froze, a.reached)
				}
			},
		},
		{
			name: "a connection that goes up and down",
			speed: func(at time.Duration) float64 {
				if at/(20*time.Second)%2 == 0 {
					return 12 * mbps
				}
				return 4 * mbps
			},
			check: func(t *testing.T, a, best outcome, l *ladder) {
				// It may try a better quality, but not with every turn.
				if a.froze > best.froze || a.changes > 6 {
					t.Errorf("froze for %v, against %v in the best quality, and changed quality %d times in ten minutes: want no more freezes, and a quality that mostly stays", a.froze, best.froze, a.changes)
				}
			},
		},
		{
			name:    "three streams on one connection",
			speed:   steady(12 * mbps),
			viewers: []time.Duration{0, 20 * time.Second, 40 * time.Second},
			check: func(t *testing.T, a, best outcome, l *ladder) {
				if a.froze > best.froze/50 || a.changes > 6 {
					t.Errorf("froze for %v, against %v in the best quality, and changed quality %d times: want the three to share what there is", a.froze, best.froze, a.changes)
				}
			},
		},
	} {
		for _, provider := range []struct {
			name    string
			ladder  *ladder
			segment time.Duration
		}{{"TVP", tvpLadder, 2 * time.Second}, {"France Télévisions", franceLadder, 7680 * time.Millisecond}} {
			t.Run(test.name+"/"+provider.name, func(t *testing.T) {
				play := func(adapts bool) (all outcome, each []string) {
					w := &world{ladder: provider.ladder, segment: provider.segment, speed: test.speed, delay: test.delay,
						start: time.Unix(0, 0), route: &route{streams: map[*controller]float64{}}}
					if w.delay == nil {
						w.delay = func(int) time.Duration { return 0 }
					}
					if adapts {
						w.note = t.Logf
					}
					for _, at := range append([]time.Duration{0}, test.viewers...)[min(len(test.viewers), 1):] {
						w.join(adapts, at)
					}
					w.run(10 * time.Minute)
					all.reached = -1
					for _, v := range w.viewers {
						all.freezes, all.changes, all.froze = all.freezes+v.freezes, all.changes+v.changes, all.froze+v.froze
						all.reached = max(all.reached, v.reached)
						each = append(each, fmt.Sprintf("%d freezes of %v, %d changes, down to %s, best quality for %v",
							v.freezes, v.froze.Round(100*time.Millisecond), v.changes, provider.ladder.qualities[v.lowest], v.top.Round(time.Second)))
					}
					return all, each
				}
				adapting, how := play(true)
				best, without := play(false)
				t.Logf("with the controller: %s", strings.Join(how, "; "))
				t.Logf("always the best:     %s", strings.Join(without, "; "))
				test.check(t, adapting, best, provider.ladder)
			})
		}
	}
}

// testController returns a controller for TVP's stream on a route, with a
// clock to be set.
func testController(r *route, now *time.Time) *controller {
	c := newController("test", tvpLadder, r, func() (time.Duration, time.Duration, bool) { return 0, 0, false })
	c.now, c.log = func() time.Time { return *now }, slog.New(slog.NewTextHandler(io.Discard, nil))
	return c
}

// A stream starts on what the speed of its connection allows, if that was
// measured of late, less what other streams on the connection take.
func TestControllerStartsOnWhatIsKnown(t *testing.T) {
	now := time.Unix(0, 0)
	r := &route{streams: map[*controller]float64{}}
	unknown := testController(r, &now)
	if got := unknown.start(); got != 2 {
		t.Errorf("with nothing known, started on quality %d, want the best", got)
	}
	unknown.end()
	r.learn(5e6, now)
	first := testController(r, &now)
	if got := first.start(); got != 1 {
		t.Errorf("at 5 Mbps, started on quality %d, want the middle one, which takes 2.7", got)
	}
	if got := testController(r, &now).start(); got != 0 {
		t.Errorf("at 5 Mbps of which 2.7 are taken, started on quality %d, want the lowest", got)
	}
	first.end()
	now = now.Add(memory)
	if got := testController(r, &now).start(); got != 2 {
		t.Errorf("at a speed measured %v ago, started on quality %d, want the best", memory, got)
	}
}

// A step up that has to be taken back is not tried again for a while, and
// for longer every time.
func TestControllerLeavesAFailedStepUpAlone(t *testing.T) {
	now := time.Unix(0, 0)
	c := testController(&route{streams: map[*controller]float64{}}, &now)
	c.start()
	step := func(to int, after time.Duration) {
		now = now.Add(after)
		c.changed(to, "test")
	}
	for _, test := range []struct {
		name     string
		up, back time.Duration // how long after the step before
		want     time.Duration // how long the best quality is then left alone
	}{
		{"a step down from where the stream started", 0, time.Minute, 0},
		{"a step up taken back at once", time.Minute, 5 * time.Second, bar},
		{"and again", 3 * time.Minute, 5 * time.Second, 2 * bar},
		{"a step down long after the step up", 5 * time.Minute, calm, 0},
	} {
		if test.up > 0 {
			step(2, test.up)
		}
		step(1, test.back)
		if got := max(c.standing[2].barred.Sub(now), 0); got != test.want {
			t.Errorf("%s: the best quality is left alone for %v, want %v", test.name, got, test.want)
		}
	}
}

// A quality that would not play is left alone for a while, not for good: it
// may have failed for the moment only.
func TestControllerTriesAQualityAgain(t *testing.T) {
	now := time.Unix(0, 0)
	c := testController(&route{streams: map[*controller]float64{}}, &now)
	c.start()
	for _, want := range []time.Duration{bar, 2 * bar} {
		c.unfits(0)
		now = now.Add(want - time.Second)
		if to, ok := c.below(0); ok {
			t.Errorf("stepped down to quality %d, which would not play, with %v to go", to, time.Second)
		}
		now = now.Add(time.Second)
		if to, ok := c.below(0); !ok || to != 0 {
			t.Errorf("%v later, below() = %d, %v: want the lowest quality tried again", want, to, ok)
		}
	}
}

// Parallel audio must not make a video segment look closer to completion.
func TestProgressUsesSegmentRate(t *testing.T) {
	r := &route{streams: map[*controller]float64{}}
	c := newController("slow video", franceLadder, r, func() (time.Duration, time.Duration, bool) { return 0, 0, true })
	quality := c.start()
	c.given, c.full, c.delivered, c.hold = 1, 10*time.Second, 5*time.Second, 0
	to, _, _, change := c.progress(quality, coming{
		got: 250000, of: 250000 + 3<<20, speed: 4e6, rate: 2e6,
		flow: time.Second, took: time.Second, doubted: time.Second, length: 8 * time.Second,
	})
	if !change || c.ladder[to].height != 540 {
		t.Fatalf("selected %d (change %t), want 540p before the reserve runs out", to, change)
	}
}

// The speed a segment tells of is that of what came after its first bytes:
// those had been waiting on the way.
func TestArrivalSpeed(t *testing.T) {
	const kB = 1 << 10
	for _, test := range []struct {
		name  string
		a     arrival
		speed float64 // in bits a second; 0 if the segment tells none
	}{
		{"a segment that takes its time", arrival{size: 1000 * kB, first: 16 * kB, flow: 2 * time.Second, fastest: 6e6}, 6e6},
		{"the same body after a long setup wait", arrival{size: 1000 * kB, first: 16 * kB, wait: 10 * time.Second, flow: 2 * time.Second, fastest: 6e6}, 6e6},
		{"a small one of which half came at once", arrival{size: 34 * kB, first: 16 * kB, flow: 48 * time.Millisecond}, 18 * kB * 8 / 0.05},
		{"one that came all at once", arrival{size: 60 * kB, first: 32 * kB, flow: time.Millisecond}, 28 * kB * 8 / 0.05},
		{"a small one that came all at once", arrival{size: 20 * kB, first: 20 * kB}, 20 * kB * 8 / 0.05},
		{"a small one that came slowly", arrival{size: 20 * kB, first: 10 * kB, flow: time.Second}, 0},
		{"next to nothing", arrival{size: 2 * kB, first: 2 * kB}, 0},
	} {
		if speed, ok := test.a.speed(); speed != test.speed || ok != (test.speed > 0) {
			t.Errorf("%s: speed() = %.0f, %t; want %.0f", test.name, speed, ok, test.speed)
		}
	}
}
