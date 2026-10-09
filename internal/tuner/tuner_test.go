package tuner

import (
	"cmp"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/combor/telesfor/internal/provider"
	"github.com/combor/telesfor/internal/remux"
)

// fake is a provider with two channels and a guide entry for each. Its
// channels can be tuned only when it has signal.
type fake struct {
	signal   bool
	manifest string // where its streams are, if anywhere real
	off      *bool  // set while it has no channels to offer
}

func (fake) Name() string { return "fake" }

func (f fake) Channels(context.Context) ([]provider.Channel, error) {
	if f.off != nil && *f.off {
		return nil, nil
	}
	return []provider.Channel{
		{ID: "one", Name: "One", Logo: "https://example.com/one.png"},
		{ID: "two", Name: "Two"},
	}, nil
}

func (fake) Programmes(context.Context, []provider.Channel, time.Time, time.Time) ([]provider.Programme, error) {
	start := time.Date(2026, 10, 3, 17, 35, 0, 0, time.FixedZone("CEST", 2*60*60))
	return []provider.Programme{
		{ChannelID: "two", Title: "News", Description: "The day's news", Image: "https://example.com/news.jpg", Start: start, Stop: start.Add(45 * time.Minute)},
		{ChannelID: "one", Title: "Film", Start: start, Stop: start.Add(45 * time.Minute)},
		{ChannelID: "gone", Title: "On a channel that is not in the lineup"},
	}, nil
}

func (f fake) Stream(context.Context, string) (provider.Source, error) {
	if !f.signal {
		return provider.Source{}, errors.New("no signal")
	}
	return provider.Source{URL: cmp.Or(f.manifest, "https://example.com/master.m3u8")}, nil
}

// device is the tuner's in most tests: at the root, numbered from 1.
var device = Device{ID: "0BADCAFE", Name: "Fake", First: 1}

// newTuner returns a tuner of device that offers the channels of p. It has no
// remuxer, so a request that gets as far as streaming panics.
func newTuner(t *testing.T, p provider.Provider) *Tuner {
	t.Helper()
	tuner, err := New(t.Context(), p, nil, device)
	if err != nil {
		t.Fatal(err)
	}
	return tuner
}

// ask sends h a request for a path, as Plex at plex.local:5004 does.
func ask(h http.Handler, method, path string) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, httptest.NewRequest(method, "http://plex.local:5004"+path, nil))
	return recorder
}

// get asks a tuner offering the fake provider's channels for a path.
func get(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()
	return ask(newTuner(t, fake{}), http.MethodGet, path)
}

// guideOf reads the XMLTV guide a tuner answered with.
func guideOf(t *testing.T, response *httptest.ResponseRecorder) xmlTV {
	t.Helper()
	var guide xmlTV
	if err := xml.Unmarshal(response.Body.Bytes(), &guide); err != nil {
		t.Fatalf("xmltv.xml = %d %s: %v", response.Code, response.Body, err)
	}
	return guide
}

// entry is a channel of lineup.json.
type entry struct{ GuideNumber, GuideName, URL string }

func TestDiscover(t *testing.T) {
	var got struct {
		BaseURL, LineupURL, DeviceID, FriendlyName string
		TunerCount                                 int
	}
	if err := json.Unmarshal(get(t, "/discover.json").Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}

	// The URLs must point back at the address the client used.
	if got.BaseURL != "http://plex.local:5004" || got.LineupURL != "http://plex.local:5004/lineup.json" {
		t.Errorf("discover.json points at %q and %q, want the request's host", got.BaseURL, got.LineupURL)
	}
	if got.DeviceID != "0BADCAFE" || got.FriendlyName != "telesfor Fake" {
		t.Errorf("discover.json names the device %q, %q", got.DeviceID, got.FriendlyName)
	}
	if got.TunerCount != 4 {
		t.Errorf("discover.json has %d tuners, want 4 of a device that names no number", got.TunerCount)
	}
}

// TestTunerAtAPath checks a tuner beside the root's: Plex is given the path as
// part of the tuner's address, so every URL it is handed has to carry it.
func TestTunerAtAPath(t *testing.T) {
	off := false
	tuner, err := New(t.Context(), fake{off: &off}, nil, Device{ID: "0BADCAFE", Name: "Fake", Path: "/fake", First: 1001, Tuners: 3})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	tuner.Register(mux)
	get := func(path string, v any) {
		t.Helper()
		recorder := ask(mux, http.MethodGet, path)
		if err := json.Unmarshal(recorder.Body.Bytes(), v); err != nil {
			t.Fatalf("%s = %d %s: %v", path, recorder.Code, recorder.Body, err)
		}
	}

	var got struct {
		BaseURL, LineupURL string
		TunerCount         int
	}
	get("/fake/discover.json", &got)
	if got.BaseURL != "http://plex.local:5004/fake" || got.LineupURL != "http://plex.local:5004/fake/lineup.json" {
		t.Errorf("discover.json points at %q and %q, want the tuner's path", got.BaseURL, got.LineupURL)
	}
	if got.TunerCount != 3 {
		t.Errorf("discover.json has %d tuners, want the device's 3", got.TunerCount)
	}
	var lineup []entry
	get("/fake/lineup.json", &lineup)
	want := []entry{
		{"1001", "One", "http://plex.local:5004/fake/stream/fake/one"},
		{"1002", "Two", "http://plex.local:5004/fake/stream/fake/two"},
	}
	if !slices.Equal(lineup, want) {
		t.Errorf("lineup.json = %v, want %v", lineup, want)
	}

	// The provider's channels change when its account does.
	off = true
	if err := tuner.Scan(t.Context()); err != nil {
		t.Fatal(err)
	}
	if get("/fake/lineup.json", &lineup); len(lineup) != 0 {
		t.Errorf("lineup.json after the provider lost its channels = %v", lineup)
	}
	if guide := guideOf(t, ask(mux, http.MethodGet, "/fake/xmltv.xml")); len(guide.Channels)+len(guide.Programmes) != 0 {
		t.Errorf("guide of an empty lineup has %d channels and %d programmes, want none", len(guide.Channels), len(guide.Programmes))
	}
}

// placed is a provider that knows where its channels stand, and has lost the
// one between these two.
type placed struct{ fake }

func (placed) Channels(context.Context) ([]provider.Channel, error) {
	return []provider.Channel{{ID: "one", Name: "One", Place: 1}, {ID: "three", Name: "Three", Place: 3}}, nil
}

func TestPlacedChannelsKeepTheirNumbers(t *testing.T) {
	tuner, err := New(t.Context(), placed{}, nil, Device{ID: "0BADCAFE", Name: "Fake", First: 1001})
	if err != nil {
		t.Fatal(err)
	}
	if want := []Station{{"1001", "One", 0}, {"1003", "Three", 0}}; !reflect.DeepEqual(tuner.Lineup(), want) {
		t.Errorf("lineup = %+v, want %+v", tuner.Lineup(), want)
	}
}

// slow is a provider that is held up the first time it lists its channels.
type slow struct {
	fake
	asked, answer chan struct{}
	held          atomic.Bool
}

func (s *slow) Channels(ctx context.Context) ([]provider.Channel, error) {
	channels, err := s.fake.Channels(ctx) // as they are when asked
	if s.held.CompareAndSwap(false, true) {
		close(s.asked)
		<-s.answer
	}
	return channels, err
}

// TestScansDoNotOvertake checks that a scan held up at the provider cannot
// publish its lineup over that of a scan started after it.
func TestScansDoNotOvertake(t *testing.T) {
	off := false
	p := &slow{fake: fake{off: &off}, asked: make(chan struct{}), answer: make(chan struct{})}
	tuner := &Tuner{provider: p, device: device}
	first, second := make(chan error, 1), make(chan error, 1)
	go func() { first <- tuner.Scan(t.Context()) }()
	<-p.asked
	off = true // the provider loses its channels, and says so
	go func() { second <- tuner.Scan(t.Context()) }()
	overtook := false
	select {
	case <-second:
		overtook = true
	case <-time.After(50 * time.Millisecond):
	}
	close(p.answer)
	<-first
	if !overtook {
		<-second
	}
	if lineup := tuner.Lineup(); len(lineup) != 0 {
		t.Errorf("lineup after the provider lost its channels = %+v, want none", lineup)
	}
}

func TestLineup(t *testing.T) {
	var got []entry
	if err := json.Unmarshal(get(t, "/lineup.json").Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}

	want := []entry{
		{"1", "One", "http://plex.local:5004/stream/fake/one"},
		{"2", "Two", "http://plex.local:5004/stream/fake/two"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("lineup.json = %v, want %v", got, want)
	}
}

func TestGuide(t *testing.T) {
	response := get(t, "/xmltv.xml")
	guide := guideOf(t, response)

	if len(guide.Channels) != 2 || guide.Channels[0].ID != "1" || guide.Channels[1].Name != "Two" {
		t.Errorf("guide channels = %+v, want the lineup with its numbers as ids", guide.Channels)
	}
	if icon := guide.Channels[0].Icon; icon == nil || icon.Src != "https://example.com/one.png" {
		t.Errorf("first channel's icon = %+v, want its logo", icon)
	}
	if guide.Channels[1].Icon != nil {
		t.Error("a channel without a logo must have no icon")
	}

	start, stop := "20261003173500 +0200", "20261003182000 +0200"
	want := []xmlProgramme{
		{
			Start:   start,
			Stop:    stop,
			Channel: "2", // the lineup number of channel "two"
			Title:   "News",
			Desc:    "The day's news",
			Icon:    &xmlIcon{"https://example.com/news.jpg"},
		},
		// A programme without an image must have no icon.
		{Start: start, Stop: stop, Channel: "1", Title: "Film"},
	}
	if !reflect.DeepEqual(guide.Programmes, want) {
		t.Errorf("guide programmes = %s, want only the two on lineup channels, the first with an icon", response.Body)
	}
}

// counting is a provider that counts the fetches of its guide, tells of each
// as it begins, and can be broken, or given a third channel.
type counting struct {
	fake
	broken  atomic.Bool
	third   atomic.Bool
	fetches atomic.Int32
	fetched chan struct{} // roomier than any test's fetches: a full one would wedge the fetcher, which sends holding t.fetching
}

func (c *counting) Channels(ctx context.Context) ([]provider.Channel, error) {
	channels, err := c.fake.Channels(ctx)
	if c.third.Load() {
		channels = append(channels, provider.Channel{ID: "three", Name: "Three"})
	}
	return channels, err
}

func (c *counting) Programmes(ctx context.Context, channels []provider.Channel, from, to time.Time) ([]provider.Programme, error) {
	c.fetches.Add(1)
	c.fetched <- struct{}{}
	if c.broken.Load() {
		return nil, errors.New("the guide is down")
	}
	return c.fake.Programmes(ctx, channels, from, to)
}

// newCounting returns a counting provider with room for every fetch a test makes.
func newCounting() *counting { return &counting{fetched: make(chan struct{}, 8)} }

// askGuide asks a tuner for its guide.
func askGuide(tuner *Tuner) *httptest.ResponseRecorder {
	return ask(tuner, http.MethodGet, "/xmltv.xml")
}

// TestGuideIsFetchedOnce checks that the guide is fetched upstream once, not
// once per request: Plex is served from the cache.
func TestGuideIsFetchedOnce(t *testing.T) {
	p := newCounting()
	tuner := newTuner(t, p)
	for range 3 {
		if guide := guideOf(t, askGuide(tuner)); len(guide.Programmes) != 2 {
			t.Fatalf("xmltv.xml has %d programmes, want two", len(guide.Programmes))
		}
	}
	if n := p.fetches.Load(); n != 1 {
		t.Errorf("three requests fetched the guide %d times, want once", n)
	}
}

// TestGuideOutlivesItsSource checks that a guide that cannot be fetched anew
// is not thrown away: Plex is served the one there is.
func TestGuideOutlivesItsSource(t *testing.T) {
	p := newCounting()
	tuner := newTuner(t, p)
	if code := askGuide(tuner).Code; code != http.StatusOK {
		t.Fatalf("xmltv.xml = %d, want 200", code)
	}
	<-p.fetched // the fetch that filled the cache

	// The source goes down, and the guide grows old enough for keepFresh to
	// want it anew.
	p.broken.Store(true)
	aged := *tuner.guide.Load()
	aged.made = aged.made.Add(-2 * guideRefresh)
	tuner.guide.Store(&aged)
	tuner.nudge <- struct{}{}
	<-p.fetched // the fetch that failed
	// The word comes as the fetch begins. Whatever the failure does to the
	// cache is done once the fetcher gives the token back.
	tuner.fetching <- struct{}{}
	<-tuner.fetching

	if guide := guideOf(t, askGuide(tuner)); len(guide.Programmes) != 2 {
		t.Errorf("xmltv.xml while the source is down has %d programmes, want the two fetched before", len(guide.Programmes))
	}
}

// stuck is a provider whose guide fetch tells when it is entered, then blocks
// until the test releases it or its context ends.
type stuck struct {
	fake
	entered chan struct{}
	release chan struct{}
}

func (s *stuck) Programmes(ctx context.Context, _ []provider.Channel, _, _ time.Time) ([]provider.Programme, error) {
	s.entered <- struct{}{}
	select {
	case <-s.release:
		return nil, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// TestCancelledGuideRequestIsNotKeptWaiting asks for the guide, with the
// request cancelled already, while the first fetch is still at the provider.
// The viewer is gone: the request must return, not wait the fetch out.
func TestCancelledGuideRequestIsNotKeptWaiting(t *testing.T) {
	p := &stuck{entered: make(chan struct{}, 8), release: make(chan struct{})}
	defer close(p.release)
	tuner := newTuner(t, p)
	<-p.entered // the first fetch is in flight

	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	returned := make(chan struct{})
	go func() {
		defer close(returned)
		tuner.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "http://plex.local:5004/xmltv.xml", nil).WithContext(cancelled))
	}()
	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("a cancelled request is kept waiting for the fetch in flight")
	}
}

// TestScanRefreshesTheGuide checks that a scan that changes the lineup has
// the guide fetched anew, before any request: Plex is not kept waiting after
// an account change.
func TestScanRefreshesTheGuide(t *testing.T) {
	p := newCounting()
	tuner := newTuner(t, p)
	<-p.fetched // the fetch that filled the cache
	p.third.Store(true)
	if err := tuner.Scan(t.Context()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.fetched:
	case <-time.After(5 * time.Second):
		t.Fatal("no guide fetch within 5s of a scan that changed the lineup")
	}
	if n := p.fetches.Load(); n != 2 {
		t.Errorf("the guide was fetched %d times in all after a scan, want two", n)
	}
}

// TestUnchangedScanKeepsTheGuide checks that a scan that changes nothing
// keeps the cached guide: what there is goes on being served, even while the
// source is down.
func TestUnchangedScanKeepsTheGuide(t *testing.T) {
	p := newCounting()
	tuner := newTuner(t, p)
	if code := askGuide(tuner).Code; code != http.StatusOK {
		t.Fatalf("xmltv.xml = %d, want 200", code)
	}
	<-p.fetched // the fetch that filled the cache

	p.broken.Store(true)
	if err := tuner.Scan(t.Context()); err != nil {
		t.Fatal(err)
	}
	if guide := guideOf(t, askGuide(tuner)); len(guide.Programmes) != 2 {
		t.Errorf("xmltv.xml after an unchanged scan has %d programmes, want the two fetched before", len(guide.Programmes))
	}
	if n := p.fetches.Load(); n != 1 {
		t.Errorf("the guide was fetched %d times though nothing changed, want once", n)
	}
}

func TestStream(t *testing.T) {
	if status := get(t, "/stream/fake/missing").Code; status != http.StatusNotFound {
		t.Errorf("unknown channel: got %d, want 404", status)
	}
	if status := get(t, "/stream/fake/one").Code; status != http.StatusServiceUnavailable {
		t.Errorf("channel that cannot be tuned: got %d, want 503", status)
	}

	// A HEAD request asks whether a channel can be tuned. It gets the answer a
	// GET would, but the channel is not tuned: nobody is there to watch.
	head := ask(newTuner(t, fake{signal: true}), http.MethodHead, "/stream/fake/one")
	if head.Code != http.StatusOK || head.Header().Get("Content-Type") != "video/mp2t" {
		t.Errorf("HEAD of a channel: got %d %q, want 200 video/mp2t", head.Code, head.Header().Get("Content-Type"))
	}
	if status := ask(newTuner(t, fake{}), http.MethodHead, "/stream/fake/one").Code; status != http.StatusServiceUnavailable {
		t.Errorf("HEAD of a channel that cannot be tuned: got %d, want 503", status)
	}
}

// TestLineupCountsStreams tunes a channel whose stream never starts, so that
// it stays open for as long as the viewer does.
func TestLineupCountsStreams(t *testing.T) {
	remuxer, err := remux.New()
	if err != nil {
		t.Skip("ffmpeg is not installed")
	}
	var once sync.Once
	asked, hold := make(chan struct{}), make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		once.Do(func() { close(asked) })
		<-hold
	}))
	defer upstream.Close()
	defer close(hold)

	tuner, err := New(t.Context(), fake{signal: true, manifest: upstream.URL + "/master.m3u8"}, remuxer, device)
	if err != nil {
		t.Fatal(err)
	}
	if want := []Station{{"1", "One", 0}, {"2", "Two", 0}}; !reflect.DeepEqual(tuner.Lineup(), want) {
		t.Fatalf("lineup = %+v, want %+v", tuner.Lineup(), want)
	}

	viewer, leave := context.WithCancel(t.Context())
	left := make(chan struct{})
	go func() {
		defer close(left)
		req := httptest.NewRequest(http.MethodGet, "http://plex.local:5004/stream/fake/two", nil)
		tuner.ServeHTTP(httptest.NewRecorder(), req.WithContext(viewer))
	}()
	<-asked
	if want := []Station{{"1", "One", 0}, {"2", "Two", 1}}; !reflect.DeepEqual(tuner.Lineup(), want) {
		t.Errorf("lineup with a viewer on Two = %+v, want %+v", tuner.Lineup(), want)
	}
	leave()
	<-left
	if streams := tuner.Lineup()[1].Streams; streams != 0 {
		t.Errorf("Two has %d streams after its viewer left, want none", streams)
	}
}
