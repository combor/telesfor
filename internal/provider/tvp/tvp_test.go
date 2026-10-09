package tvp

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/combor/telesfor/internal/provider"
)

// serve starts a fake TVP API and returns a provider that talks to it.
func serve(t *testing.T, handler http.HandlerFunc) *Provider {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return &Provider{api: server.URL, client: server.Client()}
}

// refusing starts a router of TVP's that turns every address away, as it
// does those of VPNs.
func refusing(t *testing.T) *httptest.Server {
	t.Helper()
	router := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "Forbidden.", http.StatusForbidden)
	}))
	t.Cleanup(router.Close)
	return router
}

func TestChannels(t *testing.T) {
	p := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/lives" || r.URL.Query().Get("platform") != "BROWSER" {
			t.Errorf("unexpected request: %s", r.URL)
		}
		io.WriteString(w, `{"items": [
			{"id": 399697, "title": "TVP 1", "logoImages": {"1x1": [{"url": "//s.tvp.pl/tvp1.png"}]}},
			{"id": 957438, "title": "TVP SERIALE", "payable": true, "loginRequired": true},
			{"id": 2504621, "title": "TVP NA DOBRE I NA ZŁE", "liveType": "FAST"},
			{"id": 399699, "title": "TVP INFO"}
		]}`)
	})

	got, err := p.Channels(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want := []provider.Channel{
		{ID: "399697", Name: "TVP 1", Logo: "https://s.tvp.pl/tvp1.png"},
		{ID: "399699", Name: "TVP INFO"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("Channels() = %v, want %v", got, want)
	}
}

func TestStream(t *testing.T) {
	var stream string
	p := serve(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/token/master.m3u8":
			io.WriteString(w, "#EXTM3U")
		case r.URL.Path != "/399697/videos/playlist" || r.URL.Query().Get("videoType") != "LIVE":
			t.Errorf("unexpected request: %s", r.URL)
		default:
			io.WriteString(w, `{"sources": {
				"HLS":  [{"src": "`+stream+`"}],
				"DASH": [{"src": "https://cdn.example/token/master.mpd"}]
			}}`)
		}
	})
	stream = p.api + "/token/master.m3u8"

	source, err := p.Stream(t.Context(), "399697")
	if err != nil {
		t.Fatal(err)
	}
	if source.URL != stream {
		t.Errorf("Stream() URL = %q, want %q", source.URL, stream)
	}
	if source.Client != p.client {
		t.Error("Stream() must hand out the provider's own HTTP client: the token is bound to its address")
	}
}

// TVP hands a stream out at its own server or at a router that refuses the
// addresses of VPNs. A stream that the router refuses must be played from the
// server.
func TestStreamRefusedWhereHandedOut(t *testing.T) {
	own := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "#EXTM3U")
	}))
	defer own.Close()
	router := refusing(t)

	var hosts []string // where the API hands the stream out, one call after another
	asked := 0
	p := serve(t, func(w http.ResponseWriter, r *http.Request) {
		host := hosts[min(asked, len(hosts)-1)]
		asked++
		io.WriteString(w, `{"sources": {"HLS": [{"src": "`+host+`/token/`+strconv.Itoa(asked)+`/master.m3u8?a=1"}]}}`)
	})
	p.pause = time.Millisecond
	tune := func() (string, error) {
		source, err := p.Stream(t.Context(), "399697")
		return source.URL, err
	}

	// The router alone, and no time to ask again.
	hosts = []string{router.URL}
	if got, err := tune(); err == nil || !strings.Contains(err.Error(), "refused") {
		t.Errorf("Stream() = %q, %v: want the stream refused", got, err)
	}
	// With time, it asks until TVP names its server.
	p.patience, asked = time.Minute, 0
	hosts = []string{router.URL, router.URL, own.URL}
	if got, err := tune(); err != nil || got != own.URL+"/token/3/master.m3u8?a=1" {
		t.Errorf("Stream() = %q, %v: want the third stream handed out, the first at the server", got, err)
	}
	// From then on a stream the router refuses plays from that server at once.
	asked = 0
	hosts = []string{router.URL}
	if got, err := tune(); err != nil || got != own.URL+"/token/1/master.m3u8?a=1" || asked != 1 {
		t.Errorf("Stream() = %q, %v after asking %d times: want the stream handed out, at the server, in one go", got, err, asked)
	}
}

// A viewer who leaves while a refused stream is asked for at the server must
// not make the provider forget the server.
func TestStreamCancelledKeepsTheServer(t *testing.T) {
	var hold atomic.Bool // whether the server keeps a request waiting
	arrived := make(chan struct{}, 1)
	own := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hold.Load() {
			arrived <- struct{}{}
			<-r.Context().Done()
			return
		}
		io.WriteString(w, "#EXTM3U")
	}))
	defer own.Close()
	router := refusing(t)

	var host atomic.Pointer[string] // where the API hands the stream out
	p := serve(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"sources": {"HLS": [{"src": "`+*host.Load()+`/token/master.m3u8?a=1"}]}}`)
	})

	host.Store(&own.URL)
	if _, err := p.Stream(t.Context(), "399697"); err != nil {
		t.Fatal(err)
	}

	// The viewer leaves once the server has been asked for the refused stream.
	host.Store(&router.URL)
	hold.Store(true)
	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		<-arrived
		cancel()
	}()
	if source, err := p.Stream(ctx, "399697"); err == nil {
		t.Errorf("Stream() = %q for a viewer who left, want an error", source.URL)
	}

	hold.Store(false)
	source, err := p.Stream(t.Context(), "399697")
	if err != nil || source.URL != own.URL+"/token/master.m3u8?a=1" {
		t.Errorf("Stream() = %q, %v: want the refused stream at the server, which the provider still knows", source.URL, err)
	}
}

// A host that serves no more must not take with it the one another tune has
// found in the meantime.
func TestStreamKeepsTheHostFoundMeanwhile(t *testing.T) {
	const found = "https://found.example"
	var p *Provider
	var gone atomic.Bool // whether the server has stopped serving
	own := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if gone.Load() {
			p.remember(found) // as another tune does, while this one is asking
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, "#EXTM3U")
	}))
	defer own.Close()
	router := refusing(t)

	var host atomic.Pointer[string] // where the API hands the stream out
	p = serve(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"sources": {"HLS": [{"src": "`+*host.Load()+`/token/master.m3u8?a=1"}]}}`)
	})

	host.Store(&own.URL)
	if _, err := p.Stream(t.Context(), "399697"); err != nil {
		t.Fatal(err)
	}

	host.Store(&router.URL)
	gone.Store(true)
	if source, err := p.Stream(t.Context(), "399697"); err == nil {
		t.Errorf("Stream() = %q, want the stream refused: the server serves no more", source.URL)
	}
	p.mu.Lock()
	served := p.served
	p.mu.Unlock()
	if served != found {
		t.Errorf("the host that serves is %q, want %q, found while the old one was asked", served, found)
	}
}

// A provider that has listed its channels must find out where streams are
// served before anyone tunes, so that the first stream the router refuses
// plays at once.
func TestChannelsLearnWhereStreamsAreServed(t *testing.T) {
	own := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "#EXTM3U")
	}))
	defer own.Close()
	router := refusing(t)

	var asked atomic.Int32 // the times the API was asked for a stream
	p := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/lives" {
			io.WriteString(w, `{"items": [{"id": 399699, "title": "TVP INFO"}, {"id": 399697, "title": "TVP 1"}]}`)
			return
		}
		// The API names the router, but for the third time it is asked.
		host := router.URL
		if asked.Add(1) == 3 {
			host = own.URL
		}
		io.WriteString(w, `{"sources": {"HLS": [{"src": "`+host+`/token/master.m3u8?a=1"}]}}`)
	})
	p.search, p.interval = time.Minute, time.Millisecond

	if _, err := p.Channels(t.Context()); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(time.Millisecond) {
		p.mu.Lock()
		served, learning := p.served, p.learning
		p.mu.Unlock()
		if served == own.URL && !learning {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("after asking %d times the host that serves is %q, want %q", asked.Load(), served, own.URL)
		}
	}
	if got := asked.Load(); got != 3 {
		t.Errorf("asked %d times, want it to stop once a host served the stream, the third time", got)
	}

	// The first tune is handed out at the router, and plays from the server.
	source, err := p.Stream(t.Context(), "399697")
	if err != nil || source.URL != own.URL+"/token/master.m3u8?a=1" || asked.Load() != 4 {
		t.Errorf("Stream() = %q, %v after asking %d times: want the stream at the server, in one go", source.URL, err, asked.Load())
	}

	// With a host known, listing the channels again asks for no stream.
	if _, err := p.Channels(t.Context()); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	if got := asked.Load(); got != 4 {
		t.Errorf("asked %d times after the channels were listed again, want 4: a host is known", got)
	}
}

func TestStreamRefused(t *testing.T) {
	tests := []struct {
		name     string
		status   int
		response string
		want     string // what the error must mention
	}{
		{"geo-blocked", 403, `{"code": "GEOIP_FILTER_FAILED"}`, "-tvp-proxy"},
		{"subscription", 403, `{"code": "ITEM_NOT_PAID"}`, "ITEM_NOT_PAID"},
		{"encrypted", 200, `{"sources": {"HLS": [{"src": "https://cdn.example/master.m3u8"}]}, "drm": {"WIDEVINE": {"src": "https://vod.tvp.pl/drm"}}}`, "DRM"},
		{"no HLS", 200, `{"sources": {"DASH": [{"src": "https://cdn.example/master.mpd"}]}}`, "HLS"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			p := serve(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(test.status)
				io.WriteString(w, test.response)
			})

			_, err := p.Stream(t.Context(), "1")
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Errorf("Stream() error = %v, want one mentioning %q", err, test.want)
			}
		})
	}
}

func TestProgrammes(t *testing.T) {
	windows := make(chan string, 8)
	p := serve(t, func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		if r.URL.Path != "/lives/programmes" || !slices.Equal(query["liveId[]"], []string{"399697", "399698"}) {
			t.Errorf("unexpected request: %s", r.URL)
		}
		windows <- query.Get("since") + " " + query.Get("till")
		// Every window returns the same two programmes, as the API does for a
		// programme that spans two windows.
		io.WriteString(w, `[
			{"id": 1, "title": "Teleexpress", "lead": "News", "since": "2026-10-03T17:00:00+02:00", "till": "2026-10-03T17:20:00+02:00", "live": {"id": 399697},
			 "images": {"16x9": [{"url": "//s.tvp.pl/wide.jpg"}], "3x4": [{"url": "//s.tvp.pl/upright.jpg"}]}},
			{"id": 2, "title": "Film", "lead": "A film", "description": "A long film", "since": "2026-10-03T23:00:00+02:00", "till": "2026-10-04T01:30:00+02:00", "live": {"id": 399698}}
		]`)
	})

	from := time.Date(2026, 10, 3, 15, 0, 0, 0, time.UTC)
	channels := []provider.Channel{{ID: "399697"}, {ID: "399698"}}
	got, err := p.Programmes(t.Context(), channels, from, from.Add(36*time.Hour))
	if err != nil {
		t.Fatal(err)
	}

	// 36 hours take two requests: the API serves at most 24 hours at a time.
	close(windows)
	var requested []string
	for window := range windows {
		requested = append(requested, window)
	}
	wantWindows := []string{
		"2026-10-03T15:00+0000 2026-10-04T15:00+0000",
		"2026-10-04T15:00+0000 2026-10-05T03:00+0000",
	}
	if !slices.Equal(requested, wantWindows) {
		t.Errorf("requested windows %q, want %q", requested, wantWindows)
	}

	if len(got) != 2 {
		t.Fatalf("Programmes() returned %d programmes, want 2 with duplicates dropped: %v", len(got), got)
	}
	if got[0].ChannelID != "399697" || got[0].Title != "Teleexpress" || got[0].Description != "News" {
		t.Errorf("first programme = %+v, want Teleexpress on 399697 described by its lead", got[0])
	}
	if got[0].Image != "https://s.tvp.pl/upright.jpg" || got[1].Image != "" {
		t.Errorf("programme images = %q and %q, want the first's upright one and none for the second", got[0].Image, got[1].Image)
	}
	if got[1].Description != "A long film" {
		t.Errorf("second programme description = %q, want the full description", got[1].Description)
	}
	wantStart := time.Date(2026, 10, 3, 21, 0, 0, 0, time.UTC)
	if !got[1].Start.Equal(wantStart) || got[1].Stop.Sub(got[1].Start) != 150*time.Minute {
		t.Errorf("second programme runs %v to %v, want 150 minutes from %v", got[1].Start, got[1].Stop, wantStart)
	}
}
