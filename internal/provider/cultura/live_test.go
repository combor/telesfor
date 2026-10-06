package cultura

import (
	"os"
	"strings"
	"testing"
	"time"
)

// TestLive checks the provider against TV Cultura's real streams and guide,
// which the fixtures of the other tests cannot do. Set TELESFOR_LIVE to run
// it; CI does, daily.
//
// It uses a proxy only if TELESFOR_CULTURA_PROXY names one, as CI has none.
// What TV Cultura refuses outside Brazil is then an expected result.
func TestLive(t *testing.T) {
	if os.Getenv("TELESFOR_LIVE") == "" {
		t.Skip("TELESFOR_LIVE is unset")
	}
	p, err := New(os.Getenv("TELESFOR_CULTURA_PROXY"))
	if err != nil {
		t.Fatal(err)
	}
	channels, err := p.Channels(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	t.Run("guide", func(t *testing.T) {
		from := time.Now().Truncate(time.Hour)
		to := from.Add(48 * time.Hour)
		programmes, err := p.Programmes(t.Context(), channels, from, to)
		if err != nil && strings.Contains(err.Error(), "-cultura-proxy") {
			t.Skip(err)
		}
		if err != nil {
			t.Fatal(err)
		}
		for _, channel := range channels {
			count, named := 0, 0
			covered := from // the guide has no gap up to here
			for _, programme := range programmes {
				if programme.ChannelID != channel.ID {
					continue
				}
				if programme.Title == "" || !programme.Start.Before(programme.Stop) || programme.Start.After(covered) {
					t.Errorf("programme %+v, want a title, a start before its stop and no gap after %s", programme, covered)
				}
				if count++; programme.Description != unlisted {
					named++
				}
				covered = programme.Stop
			}
			t.Logf("%s: %d programmes, %d that TV Cultura lists", channel.Name, count, named)
			if covered.Before(to) {
				t.Errorf("%s: the guide ends at %s, want it to reach %s", channel.Name, covered, to)
			}
			// Its guide listed 136 programmes in two days when this was written.
			if channel.ID == "tv-cultura" && named < 30 {
				t.Errorf("%s: TV Cultura lists %d programmes of %d, want its guide", channel.Name, named, count)
			}
		}
	})

	for _, channel := range channels {
		t.Run("stream of "+channel.Name, func(t *testing.T) {
			source, err := p.Stream(t.Context(), channel.ID)
			if err != nil && strings.Contains(err.Error(), "-cultura-proxy") {
				t.Skip(err)
			}
			// Stream has read the playlists itself.
			if err != nil || !strings.HasSuffix(source.URL, ".m3u8") {
				t.Errorf("Stream() = %+v, %v: want an HLS playlist", source, err)
			}
		})
	}
}
