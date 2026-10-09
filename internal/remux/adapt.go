package remux

import (
	"log/slog"
	"math"
	"slices"
	"sync"
	"time"
)

// What decides the quality of a stream. See controller.
const (
	// plexHold is how much of what it receives Plex holds back from its
	// player, about: that much of the stream is no reserve.
	plexHold = 5 * time.Second

	// startShare is how much of the connection's speed a quality may take to
	// be started on or stepped up to, holdShare to be stepped down to, and
	// rushShare to be stepped down to when the reserve is nearly gone and has
	// to grow again. The rest is room for the speed to vary.
	startShare = 0.7
	holdShare  = 0.8
	rushShare  = 0.5

	// probe is how long a segment is watched before the speed it comes at is
	// judged. The first of a stream is judged then if probeSize of it has
	// come, and after probeMost if not.
	probe     = 500 * time.Millisecond
	probeSize = 64 << 10
	probeMost = time.Second

	// handover is about how long it takes a lower quality to take over.
	handover = time.Second

	// minSample is the least of a segment that tells the speed, and minFlow
	// the least time it is taken to have come in: what is smaller or faster
	// tells of the delay of the connection rather than of its speed.
	minSample = 16 << 10
	minFlow   = 50 * time.Millisecond

	// calm is how long no segment may have come slowly, and the quality not
	// have changed, before it steps up, and how long a step up is then on
	// trial: six segments at least.
	calm         = 30 * time.Second
	calmSegments = 6

	// bar is how long a quality that failed its trial, or would not play, is
	// left alone. It doubles with every failure up to barMost, and starts
	// over once the quality has held for settled.
	bar     = 2 * time.Minute
	barMost = 30 * time.Minute
	settled = 5 * time.Minute

	// memory is how long the speed of a connection is taken to be what it was
	// measured at.
	memory = 10 * time.Minute
)

// route is what is known of the connection that a provider's streams come
// over: how fast it was when last measured, and what streams it carries now.
// Its zero value is ready to use.
type route struct {
	mu      sync.Mutex
	speed   float64                 // in bits a second
	at      time.Time               // when it was measured
	streams map[*controller]float64 // what each stream on it takes, in bits a second
}

// known returns the speed of the connection, if it was measured of late.
func (r *route) known(now time.Time) (float64, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.speed, r.speed > 0 && now.Sub(r.at) < memory
}

// learn notes the speed that the connection was measured at.
func (r *route) learn(speed float64, now time.Time) {
	r.mu.Lock()
	r.speed, r.at = speed, now
	r.mu.Unlock()
}

// take notes what a stream takes of the connection, and leave that it is over.
func (r *route) take(c *controller, rate float64) {
	r.mu.Lock()
	if r.streams == nil {
		r.streams = map[*controller]float64{}
	}
	r.streams[c] = rate
	r.mu.Unlock()
}

func (r *route) leave(c *controller) {
	r.mu.Lock()
	delete(r.streams, c)
	r.mu.Unlock()
}

// others returns what the other streams on the connection take together, and
// how many they are.
func (r *route) others(c *controller) (rate float64, n int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for other, takes := range r.streams {
		if other != c {
			rate, n = rate+takes, n+1
		}
	}
	return rate, n
}

// average is a moving average in which a value counts half as much for every
// half values that have come since.
type average struct{ half, sum, weight float64 }

func (a *average) add(value float64) {
	keep := math.Pow(0.5, 1/a.half)
	a.sum, a.weight = keep*a.sum+(1-keep)*value, keep*a.weight+(1-keep)
}

func (a *average) value() float64 {
	if a.weight == 0 {
		return 0
	}
	return a.sum / a.weight
}

// fetch is how long a segment plays, and how long it took to come.
type fetch struct{ length, took time.Duration }

func (f fetch) slow() bool { return f.took > f.length }

// arrival is how a segment came.
type arrival struct {
	size    int64         // of the segment, in bytes
	first   int64         // how much of that came at once, with the first of it
	wait    time.Duration // from asking for it to the first of it
	flow    time.Duration // from the first of it to the last
	fastest float64       // picture and sound together in its fastest half second, in bits a second; 0 if none was timed in full
	length  time.Duration // how long the segment plays
	newest  bool          // the stream has none after it yet
}

// speed tells how fast the connection was while a segment came: picture and
// sound together, in its fastest half second.
// A transfer starts slowly on a connection that has been idle, the slower the
// further away the provider is, and a provider may hand a segment out slower
// than the connection carries it: EBC's newest comes in three seconds, where
// its older ones take a quarter of one. So the whole of a transfer tells too
// little of the connection.
//
// Without a half second timed in full, it goes by what came after the first
// of it: what came at once had been waiting on the way. One too small to time
// tells nothing, unless all of it came at once: the connection is then as
// fast as that at least.
func (a arrival) speed() (float64, bool) {
	switch rest := a.size - a.first; {
	case a.fastest > 0:
		return a.fastest, true
	case rest >= minSample:
		return transferRate(rest, a.flow), true
	case a.size >= minSample && a.flow < minFlow:
		return transferRate(a.size, a.flow), true
	}
	return 0, false
}

func transferRate(bytes int64, elapsed time.Duration) float64 {
	return float64(bytes) * 8 / max(elapsed, minFlow).Seconds()
}

// controller decides which quality of a stream to play, so that the viewer's
// player does not run dry: the best that the connection keeps up with.
//
// It goes by two measures. The speed of the connection is what the segments
// tell as they come: the lower of a quick and a slow moving average, less
// what other streams on the same route take. The reserve is how much of the
// stream the player has in hand, which nothing tells: it is reckoned from
// what ffmpeg was given, what the viewer was sent and the time that passed.
//
// The quality steps down when segments take longer to come than to play, the
// sooner the less reserve is left, and by as many steps as it takes to fit
// the speed. It steps up one at a time, when the stream has caught up with
// what the provider sends, which is when the reserve is back, the speed leaves
// room and nothing has come slowly for a while. A step up that has to be
// taken back is not tried again for a time that grows.
//
// Only speed moves it. A provider that fails, or a viewer that is slow to
// take the stream, makes the reserve shrink as well, but no segment come
// slowly, and no quality would help.
type controller struct {
	name   string // the channel's, for the log
	ladder []quality
	route  *route
	now    func() time.Time
	sent   func() (media, since time.Duration, onAir bool) // see meter

	hold, calm time.Duration // see plexHold and calm; tests are in more of a hurry
	log        *slog.Logger

	mu sync.Mutex

	quality   int        // the one being played
	changedAt time.Time  // when it last changed
	rose      time.Time  // when it last stepped up, if it has not stepped down since
	standing  []standing // of each quality
	warned    bool       // of the lowest quality being too much

	fast   average       // of the speed, in bits a second, over the latest few segments
	slow   average       // over as long as a step up waits
	recent []fetch       // the latest segments of the quality being played, six at most
	length time.Duration // of the latest segment
	slowAt time.Time     // when a segment last came slowly

	// The reserve: see reserve.
	given     int           // how many segments ffmpeg has been given
	delivered time.Duration // how long they play
	handed    time.Duration // how long those play that ffmpeg has got or is getting
	trim      time.Duration // how much of that will not reach the viewer; -1 until it shows
	full      time.Duration // the most the reserve has been; 0 until the head start is sent
	stalled   time.Duration // how long the player has been dry, as reckoned
	dry       bool          // it is now
}

// standing is how long a quality that failed is left alone.
type standing struct {
	barred    time.Time     // until when it is not stepped up to
	unfit     time.Time     // until when it is not played at all: it would not play
	leftAlone time.Duration // for how long it was last left alone
}

func newController(name string, l *ladder, r *route, sent func() (time.Duration, time.Duration, bool)) *controller {
	return &controller{
		name: name, ladder: l.qualities, route: r, now: time.Now, sent: sent, hold: plexHold, calm: calm, log: slog.Default(),
		fast: average{half: 2}, slow: average{half: 6}, trim: -1, standing: make([]standing, len(l.qualities)),
	}
}

// start picks the quality to begin with: what the speed of the connection
// allows, if that is known, and the best if not. The first segment then
// tells: see progress.
func (c *controller) start() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.quality, c.changedAt = len(c.ladder)-1, c.now()
	why := "nothing is known of the connection yet"
	if speed, known := c.route.known(c.now()); known {
		c.sample(speed)
		c.quality = c.fits(startShare, c.budget(speed))
		why = "the connection was measured at " + megabits(speed)
	}
	c.route.take(c, c.takes(c.quality))
	c.log.Info("quality", "channel", c.name, "to", c.ladder[c.quality].String(), "reason", why)
	return c.quality
}

// end is called when the stream is over.
func (c *controller) end() {
	c.route.leave(c)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.log.Debug("remux: stream ends", "channel", c.name, "quality", c.ladder[c.quality].String(),
		"speed", megabits(c.speed()), "stalled", c.stalled.Round(100*time.Millisecond))
}

// takes is what a quality takes of the connection over time: its average
// rate, where the playlist tells one. Its segments may take half as much
// again, for which the shares leave room and the reserve is there.
func (c *controller) takes(quality int) float64 {
	if average := c.ladder[quality].average; average > 0 {
		return float64(average)
	}
	return float64(c.ladder[quality].rate)
}

func (c *controller) sample(speed float64) {
	c.fast.add(speed)
	c.slow.add(speed)
}

// speed is the speed of the connection as measured: the lower of the two
// averages, so that it falls at once and rises with care.
func (c *controller) speed() float64 { return min(c.fast.value(), c.slow.value()) }

// budget is how much of a speed is this stream's to take. A segment comes at
// the full speed of the connection when no other stream is fetching one, so
// what the others take is taken off. When they fetch at the same time, the
// speed measured is a share already.
func (c *controller) budget(speed float64) float64 {
	others, n := c.route.others(c)
	return max(speed-others, speed/float64(n+1))
}

// plays tells whether a quality is one to choose: not for a while after it
// would not play.
func (c *controller) plays(quality int) bool { return !c.now().Before(c.standing[quality].unfit) }

// fits returns the best quality that takes no more than a share of a budget,
// or the lowest if none does.
func (c *controller) fits(share, budget float64) int {
	lowest := 0
	for lowest < len(c.ladder)-1 && !c.plays(lowest) {
		lowest++
	}
	for quality := len(c.ladder) - 1; quality > lowest; quality-- {
		if c.plays(quality) && c.takes(quality) <= share*budget {
			return quality
		}
	}
	return lowest
}

// below returns the quality to step down to from the one being played, given
// the best that fits: that, but one step down at least, and the lowest there
// is at most.
func (c *controller) below(fits int) (int, bool) {
	for quality := min(fits, c.quality-1); quality >= 0; quality-- {
		if c.plays(quality) {
			return quality, true
		}
	}
	return 0, false
}

// reserve reckons how much of the stream the viewer's player has in hand:
// what ffmpeg was given, less what of that does not reach the viewer, less
// the time since the stream went on air, less what Plex holds back. Time the
// player is reckoned to have spent dry does not count: it played nothing
// then.
//
// What does not reach the viewer is the start that the aligner drops and
// what ffmpeg has yet to write. The least that this has been is the first of
// the two. It is told by all that ffmpeg has got or is getting: the viewer
// may have some of a segment before the last of it has come.
func (c *controller) reserve() time.Duration {
	media, since, onAir := c.sent()
	if !onAir {
		return 0
	}
	if behind := c.handed - media; c.trim < 0 || behind < c.trim {
		c.trim = max(behind, 0)
	}
	reserve := c.delivered - c.trim - (since - c.stalled) - c.hold
	if reserve < 0 {
		if c.full > 0 { // before that, the head start is still on its way
			if !c.dry {
				c.log.Debug("quality: the reserve has run out, as reckoned: the player may stall", "channel", c.name)
			}
			c.stalled, c.dry = c.stalled-reserve, true
		}
		return 0
	}
	c.dry = false
	return reserve
}

func (c *controller) downShare(reserve time.Duration) float64 {
	if c.full > 0 && reserve < c.full/3 {
		return rushShare
	}
	return holdShare
}

func (c *controller) calmPeriod() time.Duration { return max(c.calm, calmSegments*c.length) }

// coming is how a segment is coming, while ffmpeg waits for it.
type coming struct {
	got, of int64         // how much of how much has come, not counting what came at once with the first of it
	speed   float64       // of picture and sound together, in bits a second
	rate    float64       // of this segment alone, in bits a second
	flow    time.Duration // for how long it has been coming
	took    time.Duration // since when it was asked for
	doubted time.Duration // for how long it has looked like one to give up
	length  time.Duration // how long the segment plays
}

// progress is told how the segment that ffmpeg waits for is coming. It says
// whether the segment looks like one to give up, and when it is time to, and
// for which quality.
//
// The first segment of a stream is the test of the connection: it is given up
// if how it comes after probe shows that its quality is too much. Later ones
// are given up when they would come after the reserve has run out, once that
// has looked so for a tenth of the reserve: a segment may come slowly at
// first and then make up for it, and the more reserve there is, the longer
// there is to see. And one that has already taken longer to come than it
// plays is a slow segment, come or not: it is given up when that makes the
// quality too slow to go on with. One of which nothing has come is waited
// for: that is not the connection's doing.
func (c *controller) progress(quality int, s coming) (to int, why string, doubt, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if quality != c.quality || s.flow < probe {
		return 0, "", false, false
	}
	share, patience, speed := rushShare, time.Duration(0), s.speed
	if c.given == 0 {
		if s.flow < probeMost && s.got < probeSize {
			return 0, "", false, false
		}
		// A connection is slow at first, and half a second tells little:
		// only a quality that is plainly too much is given up for it.
		if c.takes(quality) <= c.budget(s.speed) {
			return 0, "", false, false
		}
		why = "the first probe measured " + megabits(s.speed)
	} else {
		if c.full == 0 {
			return 0, "", false, false
		}
		reserve := c.reserve()
		left := time.Duration(float64(s.of-s.got) * 8 / max(s.rate, 1) * float64(time.Second))
		// As it would be judged if it came now, and counted for the reserve.
		recent := append(slices.Clone(c.recent), fetch{s.length, s.took})
		switch {
		case s.took > s.length && c.tooSlow(recent, reserve+s.length):
			share = c.downShare(reserve + s.length)
			speed = min(speed, c.speed())
			why = "its segments take longer to come than to play"
		case s.took+left > s.length && left > reserve-handover:
			patience = min(reserve/10, 3*time.Second)
			why = "a segment was coming at " + megabits(s.rate) + ", too slowly to wait for"
		default:
			return 0, "", false, false
		}
	}
	if s.doubted < patience {
		return 0, "", true, false
	}
	// Whatever reserve there is has to grow again, and fast.
	if to, ok = c.below(c.fits(share, c.budget(speed))); ok {
		c.sample(s.speed)
	}
	return to, why, true, ok
}

// gives is told that ffmpeg is about to get a segment of the picture.
func (c *controller) gives(length time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reserve() // what ffmpeg has yet to write shows before it gets more
	c.handed += length
}

// lost is told that ffmpeg did not get all of a segment after all.
func (c *controller) lost(length time.Duration) {
	c.mu.Lock()
	c.handed -= length
	c.mu.Unlock()
}

// fetched is told of a segment that ffmpeg has been given: its quality, and
// how it came. It says whether to change quality, and to which.
func (c *controller) fetched(quality int, a arrival) (to int, why string, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now, length := c.now(), a.length
	c.given, c.delivered, c.length = c.given+1, c.delivered+length, length
	seen, timed := a.speed()
	if timed {
		// The slow average looks back as far as a step up waits, however
		// short the segments are.
		if length > 0 {
			c.slow.half = max(calmSegments, c.calm.Seconds()/length.Seconds())
		}
		c.sample(seen)
		c.route.learn(c.speed(), now)
	}
	if quality != c.quality { // the last of a quality that is being left
		return 0, "", false
	}

	latest := fetch{length, a.wait + a.flow}
	if c.recent = append(c.recent, latest); len(c.recent) > calmSegments {
		c.recent = c.recent[1:]
	}
	if latest.slow() {
		c.slowAt = now
	}
	reserve := c.reserve()
	if _, _, onAir := c.sent(); onAir && c.given >= headStart {
		c.full = max(c.full, reserve)
	}
	if !c.rose.IsZero() && now.Sub(c.rose) >= settled {
		c.standing[quality].leftAlone = 0
	}

	if c.tooSlow(c.recent, reserve) {
		// The averages take a while to come down. The segment that has just
		// come slowly tells where to: as far down as it takes, in one step.
		speed := c.speed()
		if timed && latest.slow() {
			speed = min(speed, seen)
		}
		if to, ok = c.below(c.fits(c.downShare(reserve), c.budget(speed))); ok {
			return to, "its segments take longer to come than to play", true
		}
		if !c.warned {
			c.warned = true
			c.log.Warn("quality: the connection is too slow for the lowest quality, so the stream will stall",
				"channel", c.name, "quality", c.ladder[quality].String(), "speed", megabits(c.speed()))
		}
		return 0, "", false
	}
	quiet := now.Sub(c.slowAt) >= c.calmPeriod()
	if c.warned && quiet {
		c.warned = false
	}

	next := quality + 1
	for next < len(c.ladder) && !c.plays(next) {
		next++
	}
	// With the newest segment in, the reserve is as large as it gets.
	if next < len(c.ladder) && a.newest && c.full > 0 && reserve >= c.full*2/3 && !now.Before(c.standing[next].barred) &&
		c.takes(next) <= startShare*c.budget(c.speed()) &&
		quiet && now.Sub(c.changedAt) >= c.calmPeriod() {
		return next, "the connection has room for more", true
	}
	return 0, "", false
}

// holds tells whether a segment is to be kept from ffmpeg until all of it has
// come, so that it can be given up: once the head start is in, and there is a
// reserve to tell by.
func (c *controller) holds() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.full > 0
}

// tooSlow tells whether recent, the latest segments of the quality being
// played, come too slowly to go on with it. How much it takes to say so
// depends on the reserve: with most of it left, they must together have taken
// longer to come than they play, so that one slow segment changes nothing.
// With less, two slow ones in a row, and with little, a single one.
func (c *controller) tooSlow(recent []fetch, reserve time.Duration) bool {
	n := len(recent)
	switch {
	case n == 0:
		return false
	case c.full > 0 && reserve < c.full/3:
		return recent[n-1].slow()
	case c.full == 0 || reserve < c.full*2/3:
		return n >= 2 && recent[n-1].slow() && recent[n-2].slow()
	}
	var all fetch
	for _, f := range recent {
		all.length, all.took = all.length+f.length, all.took+f.took
	}
	return n >= calmSegments/2 && all.slow()
}

// changed is told that the quality is changing, and why.
func (c *controller) changed(to int, why string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now, from := c.now(), c.quality
	if to < from && !c.rose.IsZero() && now.Sub(c.rose) < c.calmPeriod() {
		c.standing[from].barred = c.leaveAlone(from, now)
	}
	c.rose = time.Time{}
	if to > from {
		c.rose = now
	}
	c.quality, c.changedAt, c.recent = to, now, nil
	c.route.take(c, c.takes(to))
	c.log.Info("quality", "channel", c.name, "from", c.ladder[from].String(), "to", c.ladder[to].String(), "reason", why,
		"speed", megabits(c.speed()), "reserve", c.reserve().Round(100*time.Millisecond))
}

// unfits is told of a quality that would not play, to leave it alone for a
// while, and for longer every time: it may have failed for the moment only.
func (c *controller) unfits(quality int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.standing[quality].unfit = c.leaveAlone(quality, c.now())
	c.rose = time.Time{} // a step up to it was no trial of the connection
}

func (c *controller) leaveAlone(quality int, now time.Time) time.Time {
	s := &c.standing[quality]
	s.leftAlone = min(max(2*s.leftAlone, bar), barMost)
	return now.Add(s.leftAlone)
}
