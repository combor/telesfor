package tvp

import (
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
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
	p := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/399697/videos/playlist" || r.URL.Query().Get("videoType") != "LIVE" {
			t.Errorf("unexpected request: %s", r.URL)
		}
		io.WriteString(w, `{"sources": {
			"HLS":  [{"src": "https://cdn.example/token/master.m3u8"}],
			"DASH": [{"src": "https://cdn.example/token/master.mpd"}]
		}}`)
	})

	source, err := p.Stream(t.Context(), "399697")
	if err != nil {
		t.Fatal(err)
	}
	if want := "https://cdn.example/token/master.m3u8"; source.URL != want {
		t.Errorf("Stream() URL = %q, want %q", source.URL, want)
	}
	if source.Client != p.client {
		t.Error("Stream() must hand out the provider's own HTTP client: the token is bound to its address")
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
