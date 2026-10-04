package tvp

import (
	"io"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/combor/telesfor/internal/provider"
)

// TestLive checks the provider against TVP's real API, which the fixtures of
// the other tests cannot do. Set TELESFOR_LIVE to run it; CI does, daily.
//
// It uses no proxy, as CI has none. The channel list and the guide are open to
// the world, and a stream that TVP refuses outside Poland is an expected
// result.
func TestLive(t *testing.T) {
	if os.Getenv("TELESFOR_LIVE") == "" {
		t.Skip("TELESFOR_LIVE is unset")
	}
	p, err := New("")
	if err != nil {
		t.Fatal(err)
	}

	channels, err := p.Channels(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%d channels", len(channels))
	if len(channels) < 20 { // 42 when this was written
		t.Fatalf("only %d channels: %v", len(channels), channels)
	}
	ids := map[string]bool{}
	for _, channel := range channels {
		ids[channel.ID] = true
		if channel.ID == "" || channel.Name == "" || !strings.HasPrefix(channel.Logo, "https://") {
			t.Errorf("channel %+v, want an id, a name and a logo", channel)
		}
	}

	t.Run("guide", func(t *testing.T) {
		from := time.Now().Truncate(time.Hour)
		programmes, err := p.Programmes(t.Context(), channels, from, from.Add(48*time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		posters := 0
		for _, programme := range programmes {
			if programme.Title == "" || !ids[programme.ChannelID] || !programme.Start.Before(programme.Stop) {
				t.Errorf("programme %+v, want a title, a channel of the lineup and a start before its stop", programme)
			}
			if strings.HasPrefix(programme.Image, "https://") {
				posters++
			}
		}
		t.Logf("%d programmes, %d with a poster", len(programmes), posters)
		if len(programmes) < len(channels) || posters < len(programmes)/2 {
			t.Errorf("%d programmes for %d channels, %d with a poster: want a guide, mostly with posters",
				len(programmes), len(channels), posters)
		}
	})

	// TVP INFO is the channel TVP is most likely to serve abroad.
	t.Run("stream", func(t *testing.T) {
		i := slices.IndexFunc(channels, func(channel provider.Channel) bool { return channel.Name == "TVP INFO" })
		if i < 0 {
			t.Fatal("TVP INFO is not in the lineup")
		}
		source, err := p.Stream(t.Context(), channels[i].ID)
		if err != nil && strings.Contains(err.Error(), "blocked outside Poland") {
			t.Skip(err)
		}
		if err != nil {
			t.Fatal(err)
		}

		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, source.URL, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := source.Client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		manifest, err := io.ReadAll(resp.Body)
		if err != nil || resp.StatusCode != http.StatusOK || !strings.HasPrefix(string(manifest), "#EXTM3U") {
			t.Errorf("manifest: %s, %v, starting %.40q: want an HLS playlist", resp.Status, err, manifest)
		}
	})
}
