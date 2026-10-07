package francetv

import (
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

// TestLive checks the provider against France Télévisions' real APIs and
// streams, which the fixtures of the other tests cannot do. Set TELESFOR_LIVE
// to run it; CI does, daily.
//
// It uses a proxy only if TELESFOR_FRANCETV_PROXY names one, as CI has none.
// What France Télévisions refuses outside France is then an expected result.
func TestLive(t *testing.T) {
	if os.Getenv("TELESFOR_LIVE") == "" {
		t.Skip("TELESFOR_LIVE is unset")
	}
	p, err := New(os.Getenv("TELESFOR_FRANCETV_PROXY"))
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
		if err != nil && strings.Contains(err.Error(), "-francetv-proxy") {
			t.Skip(err)
		}
		if err != nil {
			t.Fatal(err)
		}
		p.mu.Lock()
		t.Logf("%d programmes looked up at %d paths", len(p.shows), len(p.asked))
		p.mu.Unlock()
		for _, channel := range channels {
			count, listed, unnamed := 0, 0, 0
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
				// What the guide calls an episode whose programme it does
				// not name.
				if strings.HasPrefix(programme.Title, "Émission du ") || strings.HasPrefix(programme.Title, "Édition du ") {
					unnamed++
				}
				covered = programme.Stop
			}
			t.Logf("%s: %d programmes, %d that France Télévisions lists, %d without the name of their programme", channel.Name, count, listed, unnamed)
			if covered.Before(to) {
				t.Errorf("%s: the guide ends at %s, want it to reach %s", channel.Name, covered, to)
			}
			// franceinfo, which has the fewest, listed 35 programmes in two
			// days when this was written, and none went by its episode.
			if listed < 20 || unnamed > listed/10 {
				t.Errorf("%s: %d programmes listed of %d, %d of them unnamed: want its guide, by the names of its programmes", channel.Name, listed, count, unnamed)
			}
		}
	})

	for _, channel := range channels {
		t.Run("stream of "+channel.Name, func(t *testing.T) {
			source, err := p.Stream(t.Context(), channel.ID)
			if err != nil && strings.Contains(err.Error(), "-francetv-proxy") {
				t.Skip(err)
			}
			if err != nil {
				t.Fatal(err)
			}
			// Stream has read the playlists itself. What is played is the
			// master, with the qualities to choose from and their sound.
			resp, err := source.Client.Get(source.URL)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			master, _ := io.ReadAll(resp.Body)
			if strings.Count(string(master), "#EXT-X-STREAM-INF:") < 2 || !strings.Contains(string(master), "TYPE=AUDIO") {
				t.Errorf("the master playlist, want more than one quality and the sound:\n%s", master)
			}
		})
	}
}
