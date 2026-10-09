package tf1

import (
	"os"
	"strings"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/combor/telesfor/internal/provider"
	"github.com/combor/telesfor/internal/provider/providertest"
	"github.com/combor/telesfor/internal/store"
)

// TestLive checks the provider against TF1's real APIs, guide and streams,
// which the fixtures of the other tests cannot do. Set TELESFOR_LIVE to run
// it; CI does, daily, for what takes neither an account nor a French
// address: the guide, and LCI.
//
// The other channels need a telesfor that has signed in: TELESFOR_DATA is its
// data directory, which that telesfor must not be using meanwhile, and
// TELESFOR_TF1_PROXY its proxy.
func TestLive(t *testing.T) {
	if os.Getenv("TELESFOR_LIVE") == "" {
		t.Skip("TELESFOR_LIVE is unset")
	}
	var db *bolt.DB
	if dir := os.Getenv("TELESFOR_DATA"); dir != "" {
		var err error
		if db, err = store.Open(dir); err != nil {
			t.Fatal(err)
		}
		defer db.Close()
	}
	p, err := New(os.Getenv("TELESFOR_TF1_PROXY"), db)
	if err != nil {
		t.Fatal(err)
	}
	playing, err := p.Channels(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if db != nil && len(playing) != len(channels) {
		t.Fatalf("%d channels, want %d: is this telesfor signed in? %v", len(playing), len(channels), playing)
	}

	// The guide is there for anyone, whatever plays.
	t.Run("guide", func(t *testing.T) {
		var all []provider.Channel
		for _, ch := range channels {
			all = append(all, provider.Channel{ID: ch.id, Name: ch.name})
		}
		from := time.Now().Truncate(time.Hour)
		to := from.Add(48 * time.Hour)
		guide, err := p.Programmes(t.Context(), all, from, to)
		if err != nil {
			t.Fatal(err)
		}
		if login := p.Login(); db != nil && login.State != provider.SignedIn {
			t.Errorf("after the guide, the sign-in stands at %+v: want it accepted", login)
		}
		for i, ch := range channels {
			programmes := providertest.Covered(t, guide, all[i], from, to)
			listed, pictures := 0, 0
			for _, programme := range programmes {
				if programme.Description != unlisted {
					listed++
				}
				if strings.HasPrefix(programme.Image, "https://") {
					pictures++
				}
			}
			t.Logf("%s: %d programmes, %d that TF1 lists, %d with a picture", ch.name, len(programmes), listed, pictures)
			// TFX, which has the fewest, listed 43 programmes in two days
			// when this was written.
			if ch.guide != "" && (listed < 25 || pictures < listed/2) || ch.guide == "" && listed > 0 {
				t.Errorf("%s: %d programmes listed of %d, %d with a picture: want its guide, if TF1 publishes one", ch.name, listed, len(programmes), pictures)
			}
		}
	})

	for _, channel := range playing {
		t.Run("stream of "+channel.Name, func(t *testing.T) {
			source, err := p.Stream(t.Context(), channel.ID)
			if err != nil && strings.Contains(err.Error(), "-tf1-proxy") {
				t.Skip(err)
			}
			if err != nil {
				t.Fatal(err)
			}
			// What is played is the master, with the qualities to choose
			// from and their sound.
			_, master := providertest.Get(t, source.Client, source.URL)
			if strings.Count(master, "#EXT-X-STREAM-INF:") < 2 || !strings.Contains(master, "TYPE=AUDIO") ||
				strings.Contains(master, "KEY") {
				t.Errorf("the master playlist, want more than one quality, the sound and no key:\n%s", master)
			}
		})
	}
}
