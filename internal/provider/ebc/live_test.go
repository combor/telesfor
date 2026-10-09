package ebc

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/combor/telesfor/internal/provider/providertest"
)

// TestLive checks the provider against EBC's real streams and guide, which the
// fixtures of the other tests cannot do. Set TELESFOR_LIVE to run it; CI does,
// daily.
//
// It uses a proxy only if TELESFOR_EBC_PROXY names one, as CI has none. What
// EBC refuses outside Brazil is then an expected result.
func TestLive(t *testing.T) {
	if os.Getenv("TELESFOR_LIVE") == "" {
		t.Skip("TELESFOR_LIVE is unset")
	}
	p, err := New(os.Getenv("TELESFOR_EBC_PROXY"))
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
		guide, err := p.Programmes(t.Context(), channels, from, to)
		if err != nil && strings.Contains(err.Error(), "-ebc-proxy") {
			t.Skip(err)
		}
		if err != nil {
			t.Fatal(err)
		}
		for _, channel := range channels {
			programmes := providertest.Covered(t, guide, channel, from, to)
			named := 0
			for _, programme := range programmes {
				if programme.Description == "" {
					named++
				}
			}
			t.Logf("%s: %d programmes, %d that EBC names", channel.Name, len(programmes), named)
			// TV Brasil's own guide named 80 programmes in two days when this was written.
			if channel.ID == "tv-brasil" && named < 30 {
				t.Errorf("TV Brasil: EBC names %d programmes of %d, want its guide", named, len(programmes))
			}
		}
	})

	for _, channel := range channels {
		t.Run("stream of "+channel.Name, func(t *testing.T) {
			source, err := p.Stream(t.Context(), channel.ID)
			if err != nil && strings.Contains(err.Error(), "-ebc-proxy") {
				t.Skip(err)
			}
			// Stream has read the playlists itself.
			if err != nil || !strings.HasSuffix(source.URL, ".m3u8") {
				t.Errorf("Stream() = %+v, %v: want an HLS playlist", source, err)
			}
		})
	}
}
