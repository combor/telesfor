package tuner

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/combor/telesfor/internal/provider"
)

// fake is a provider with two channels and a guide entry for each. Its
// channels can be tuned only when it has signal.
type fake struct{ signal bool }

func (fake) Name() string { return "fake" }

func (fake) Channels(context.Context) ([]provider.Channel, error) {
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
	return provider.Source{URL: "https://example.com/master.m3u8"}, nil
}

// request sends a request to a tuner offering the channels of p. The tuner has
// no remuxer, so a request that gets as far as streaming panics.
func request(t *testing.T, p provider.Provider, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	tuner, err := New(t.Context(), []provider.Provider{p}, nil)
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	tuner.ServeHTTP(recorder, httptest.NewRequest(method, "http://plex.local:5004"+path, nil))
	return recorder
}

// get asks a tuner offering the fake provider's channels for a path.
func get(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()
	return request(t, fake{}, http.MethodGet, path)
}

func TestDiscover(t *testing.T) {
	var device struct{ BaseURL, LineupURL, DeviceID string }
	if err := json.Unmarshal(get(t, "/discover.json").Body.Bytes(), &device); err != nil {
		t.Fatal(err)
	}

	// The URLs must point back at the address the client used.
	if device.BaseURL != "http://plex.local:5004" || device.LineupURL != "http://plex.local:5004/lineup.json" {
		t.Errorf("discover.json points at %q and %q, want the request's host", device.BaseURL, device.LineupURL)
	}
	if device.DeviceID == "" {
		t.Error("discover.json has no DeviceID")
	}
}

func TestLineup(t *testing.T) {
	type entry struct{ GuideNumber, GuideName, URL string }
	var got []entry
	if err := json.Unmarshal(get(t, "/lineup.json").Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}

	want := []entry{
		{"1", "One", "http://plex.local:5004/stream/fake/one"},
		{"2", "Two", "http://plex.local:5004/stream/fake/two"},
	}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("lineup.json = %v, want %v", got, want)
	}
}

func TestGuide(t *testing.T) {
	response := get(t, "/xmltv.xml")
	var guide xmlTV
	if err := xml.Unmarshal(response.Body.Bytes(), &guide); err != nil {
		t.Fatalf("xmltv.xml is not valid XML: %v\n%s", err, response.Body)
	}

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

func TestStream(t *testing.T) {
	if status := get(t, "/stream/fake/missing").Code; status != http.StatusNotFound {
		t.Errorf("unknown channel: got %d, want 404", status)
	}
	if status := get(t, "/stream/fake/one").Code; status != http.StatusServiceUnavailable {
		t.Errorf("channel that cannot be tuned: got %d, want 503", status)
	}

	// A HEAD request asks whether a channel can be tuned. It gets the answer a
	// GET would, but the channel is not tuned: nobody is there to watch.
	head := request(t, fake{signal: true}, http.MethodHead, "/stream/fake/one")
	if head.Code != http.StatusOK || head.Header().Get("Content-Type") != "video/mp2t" {
		t.Errorf("HEAD of a channel: got %d %q, want 200 video/mp2t", head.Code, head.Header().Get("Content-Type"))
	}
	if status := request(t, fake{}, http.MethodHead, "/stream/fake/one").Code; status != http.StatusServiceUnavailable {
		t.Errorf("HEAD of a channel that cannot be tuned: got %d, want 503", status)
	}
}
