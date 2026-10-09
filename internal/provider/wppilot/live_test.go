package wppilot

import (
	"context"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/combor/telesfor/internal/provider"
	"github.com/combor/telesfor/internal/provider/providertest"
	"github.com/combor/telesfor/internal/store"
)

// TestLive checks the provider against WP Pilot's real API, which the
// fixtures of the other tests cannot do. Set TELESFOR_LIVE to run it.
//
// It needs a telesfor that has signed in: TELESFOR_DATA is its data directory,
// which that telesfor must not be using meanwhile, and TELESFOR_WPPILOT_PROXY
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
	p, err := New(os.Getenv("TELESFOR_WPPILOT_PROXY"), db)
	if err != nil {
		t.Fatal(err)
	}

	channels, err := p.Channels(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	// A free account got 33 when this was written.
	if len(channels) < 10 {
		t.Fatalf("%d channels: is this telesfor signed in? %v", len(channels), channels)
	}
	for _, channel := range channels {
		if channel.Name == "" || !strings.HasPrefix(channel.Logo, "https://") || channel.Place == 0 {
			t.Errorf("channel %+v, want a name, a logo and a place", channel)
		}
	}

	t.Run("guide", func(t *testing.T) {
		from := time.Now().Truncate(time.Hour)
		to := from.Add(48 * time.Hour)
		programmes, err := p.Programmes(t.Context(), channels, from, to)
		if err != nil {
			t.Fatal(err)
		}
		if login := p.Login(); login != (provider.Login{State: provider.SignedIn}) {
			t.Errorf("after the guide, the sign-in stands at %+v: want it accepted, and nothing in its way", login)
		}
		with := 0 // channels that WP lists programmes of
		for _, channel := range channels {
			count, listed, pictures := 0, 0, 0
			covered := from // the guide has no gap up to here
			for _, programme := range programmes {
				if programme.ChannelID != channel.ID {
					continue
				}
				if programme.Title == "" || !programme.Start.Before(programme.Stop) || programme.Start.After(covered) {
					t.Errorf("programme %+v, want a title, a start before its stop and no gap after %s", programme, covered)
				}
				if count++; programme.Description != unlisted {
					listed++
				}
				if strings.HasPrefix(programme.Image, "https://") {
					pictures++
				}
				if programme.Stop.After(covered) {
					covered = programme.Stop
				}
			}
			t.Logf("%s: %d programmes, %d that WP lists, %d with a picture", channel.Name, count, listed, pictures)
			if covered.Before(to) {
				t.Errorf("%s: the guide ends at %s, want it to reach %s", channel.Name, covered, to)
			}
			if listed > 0 {
				with++
			}
		}
		// All but its fireplace had one when this was written.
		if with < len(channels)*3/4 {
			t.Errorf("WP lists programmes of %d channels in %d, want most", with, len(channels))
		}
	})

	// Three are enough: WP counts every one as a change of channel.
	for _, channel := range channels[:3] {
		t.Run("stream of "+channel.Name, func(t *testing.T) {
			ctx, leave := context.WithCancel(t.Context())
			defer func() {
				leave()
				p.watching.Wait() // for the stream's session to be closed at WP
			}()
			source, err := p.Stream(ctx, channel.ID)
			if err != nil && strings.Contains(err.Error(), "-wppilot-proxy") {
				t.Skip(err)
			}
			if err != nil {
				t.Fatal(err)
			}
			// What is played is the master, with the qualities to choose
			// from and their sound. The first quality in it tells whether
			// the stream can be played.
			resp, master := providertest.Get(t, source.Client, source.URL)
			if strings.Count(master, "#EXT-X-STREAM-INF:") < 2 || !strings.Contains(master, "TYPE=AUDIO") {
				t.Fatalf("the master playlist: %s, want more than one quality and the sound:\n%s", resp.Status, master)
			}
			_, quality, _ := strings.Cut(master, "#EXT-X-STREAM-INF:")
			_, quality, _ = strings.Cut(quality, "\n") // after its attributes
			quality, _, _ = strings.Cut(quality, "\n")
			address, err := resp.Request.URL.Parse(strings.TrimSpace(quality))
			if err != nil {
				t.Fatal(err)
			}
			resp, playlist := providertest.Get(t, source.Client, address.String())
			if resp.StatusCode != http.StatusOK || !strings.Contains(playlist, "#EXTINF") || strings.Contains(playlist, "#EXT-X-KEY") {
				t.Errorf("playlist: %s, starting %.40q: want an HLS playlist of one quality, in the clear", resp.Status, playlist)
			}
		})
	}
}
