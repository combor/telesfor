package globo

import (
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/combor/telesfor/internal/provider"
	"github.com/combor/telesfor/internal/store"
)

// TestLive checks the provider against Globo's real APIs, which the fixtures
// of the other tests cannot do. Set TELESFOR_LIVE to run it.
//
// It needs a telesfor that has signed in: TELESFOR_DATA is its data directory,
// which that telesfor must not be using meanwhile, and TELESFOR_GLOBO_PROXY
// its proxy. CI has neither.
func TestLive(t *testing.T) {
	if os.Getenv("TELESFOR_LIVE") == "" || os.Getenv("TELESFOR_DATA") == "" {
		t.Skip("TELESFOR_LIVE or TELESFOR_DATA is unset")
	}
	db, err := store.Open(os.Getenv("TELESFOR_DATA"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	p, err := New(os.Getenv("TELESFOR_GLOBO_PROXY"), db)
	if err != nil {
		t.Fatal(err)
	}

	channels, err := p.Channels(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(channels) != len(slugs) {
		t.Fatalf("%d channels, want %d: is this telesfor signed in? %v", len(channels), len(slugs), channels)
	}
	for _, channel := range channels {
		if channel.Name == "" || !strings.HasPrefix(channel.Logo, "https://") {
			t.Errorf("channel %+v, want a name and a logo", channel)
		}
	}

	t.Run("guide", func(t *testing.T) {
		from := time.Now().Truncate(time.Hour)
		to := from.Add(48 * time.Hour)
		programmes, err := p.Programmes(t.Context(), channels, from, to)
		if err != nil {
			t.Fatal(err)
		}
		if login := p.Login(); login.State != provider.SignedIn {
			t.Errorf("after the guide, the sign-in stands at %+v: want it accepted", login)
		}
		for _, channel := range channels {
			count, posters := 0, 0
			var first, last time.Time
			for _, programme := range programmes {
				if programme.ChannelID != channel.ID {
					continue
				}
				if programme.Title == "" || !programme.Start.Before(programme.Stop) {
					t.Errorf("programme %+v, want a title and a start before its stop", programme)
				}
				if count++; count == 1 {
					first = programme.Start
				}
				if last = programme.Stop; strings.HasPrefix(programme.Image, "https://") {
					posters++
				}
			}
			t.Logf("%s: %d programmes, %d with a poster, from %s to %s", channel.Name, count, posters,
				first.Format("Mon 15:04"), last.Format("Mon 15:04"))
			// ge tv announces less than two days.
			if count == 0 || first.After(from) || last.Before(from.Add(24*time.Hour)) {
				t.Errorf("%s: the guide runs from %s to %s, want it to cover a day from %s", channel.Name, first, last, from)
			}
		}
	})

	for _, channel := range channels {
		t.Run("stream of "+channel.Name, func(t *testing.T) {
			source, err := p.Stream(t.Context(), channel.ID)
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
			playlist, err := io.ReadAll(resp.Body)
			if err != nil || resp.StatusCode != http.StatusOK || !strings.HasPrefix(string(playlist), "#EXTM3U") ||
				!strings.Contains(string(playlist), "#EXTINF") || strings.Contains(string(playlist), "#EXT-X-KEY") {
				t.Errorf("playlist: %s, %v, starting %.40q: want an HLS playlist of one quality, in the clear", resp.Status, err, playlist)
			}
		})
	}
}
