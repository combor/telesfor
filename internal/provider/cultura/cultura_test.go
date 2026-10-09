package cultura

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/combor/telesfor/internal/provider"
)

// The playlists of a channel, as Cultura Fast's are laid out: one quality,
// with the sound in it, at an address made for whoever asks.
const (
	master = `#EXTM3U
#EXT-X-VERSION:3
#EXT-X-STREAM-INF:BANDWIDTH=1712199,RESOLUTION=1280x720,CODECS="avc1.4d401f,mp4a.40.2"
media.m3u8?session=SESSION
`
	head = `#EXTM3U
#EXT-X-VERSION:3
#EXT-X-TARGETDURATION:3
#EXT-X-MEDIA-SEQUENCE:362303
`
	segment = `#EXTINF:3.003000,
output_0_362303.ts?session=SESSION
`
)

// entry is a programme in a day of the guide, as TV Cultura's site writes it.
// One without a picture has the site's own, and the episode is left out
// altogether when there is none.
func entry(clock, name, picture, episode, synopsis string) string {
	if picture == "" {
		picture = "/_img/tvcultura/programas_abertura_TV.jpg"
	}
	if episode != "" {
		episode = "<h2>" + episode + " </h2>"
	}
	return `<section class="grade-box infantil" id="">
	<div>
		<a title="` + name + `">
			<img src="` + picture + `" alt="` + name + `" width="331">
			<time>` + clock + `</time>
		</a>
	</div>
	<div class="info-programa">
		<h2><a href="#mais" data-more-info="true" title="` + name + `">Infantil</a></h2>
		<h3><a href="#mais" data-more-info="true" title="` + name + `">` + name + ` </a></h3>
		<div class="icones"><img src="/_img/tvcultura/icones/al.png" alt="al" /></div>
		<a href="#mais"><img src="/_img/tvcultura/icones/seta-cinza.png" /></a>
	</div>
	<section class="mais">
		<section>
			` + episode + `
			<div>
				` + synopsis + `
			</div>
		</section>
		<img class="icon-mobile" src="/_img/tvcultura/icones/al.png" alt="al" />
	</section>
</section>
`
}

// cultura is a fake of TV Cultura: its guide, and the playlists of a channel.
type cultura struct {
	days   map[string]string // the guide: the entries of a day, by its date
	asked  chan string       // the days of the guide asked for
	status int               // if set, how the guide and the master playlist answer
	master string
	media  string // the playlist of the one quality
}

// serve starts a fake TV Cultura and returns it with a provider that talks to
// it: TV Cultura, which has a guide, and Cultura Fast, which has none.
func serve(t *testing.T) (*cultura, *Provider) {
	t.Helper()
	c := &cultura{asked: make(chan string, 16), master: master, media: head + segment}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /grade/{page}", func(w http.ResponseWriter, r *http.Request) {
		day := strings.TrimSuffix(r.PathValue("page"), ".html")
		c.asked <- day
		entries, published := c.days[day]
		switch {
		case c.status != 0:
			w.WriteHeader(c.status)
		case !published:
			http.NotFound(w, r)
		default:
			io.WriteString(w, `<section class="programas">`+entries+`</section>`)
		}
	})
	mux.HandleFunc("GET /live/{channel}/master.m3u8", func(w http.ResponseWriter, r *http.Request) {
		if c.status != 0 {
			w.WriteHeader(c.status)
			return
		}
		io.WriteString(w, c.master)
	})
	mux.HandleFunc("GET /live/{channel}/media.m3u8", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, c.media)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	return c, &Provider{client: server.Client(), channels: []channel{
		{id: "tv-cultura", name: "TV Cultura", stream: server.URL + "/live/tvcultura/master.m3u8", guide: server.URL + "/grade"},
		{id: "cultura-fast", name: "Cultura Fast", stream: server.URL + "/live/fast/master.m3u8"},
	}}
}

func TestChannels(t *testing.T) {
	p, err := New("")
	if err != nil {
		t.Fatal(err)
	}
	got, err := p.Channels(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for i, ch := range got {
		names = append(names, ch.Name)
		if ch.ID == "" || ch.Place != i+1 {
			t.Errorf("channel %+v, want an id and place %d", ch, i+1)
		}
	}
	if want := []string{"TV Cultura", "Cultura Fast"}; !slices.Equal(names, want) {
		t.Errorf("Channels() = %q, want %q", names, want)
	}
}

func TestProgrammes(t *testing.T) {
	const poster = "https://cultura.uol.com.br/upload/tvcultura/programas/schedule_med/rodaviva.jpeg"
	c, p := serve(t)
	c.days = map[string]string{
		// A day runs into the next morning.
		"04102026": entry("23:30", "Cultura Livre", "", "", "") +
			entry("02:00", "Balaio", "", "", ""),
		"05102026": entry("05:58", "ABERTURA DA EMISSORA", "", "", "") +
			entry("06:02", "IURI & UDI", "", "IURI &amp; UDI  -  14", "Lorde Rodolfus reúne os vilões.\r\n\r\n\r\n  E os   espiões?") +
			entry("22:00", "Roda Viva", poster, "CARLOS MELO", "") +
			entry("00:03", "Persona", "", "", ""),
		// TV Cultura has yet to publish the 6th.
	}

	// Two days from midnight in Brasília.
	from := time.Date(2026, 10, 5, 3, 0, 0, 0, time.UTC)
	channels, _ := p.Channels(t.Context())
	programmes, err := p.Programmes(t.Context(), channels, from, from.Add(48*time.Hour))
	if err != nil {
		t.Fatal(err)
	}

	// The day before the two as well, for what runs over midnight.
	close(c.asked)
	var asked []string
	for day := range c.asked {
		asked = append(asked, day)
	}
	if want := []string{"04102026", "05102026", "06102026"}; !slices.Equal(asked, want) {
		t.Errorf("asked for the days %q, want %q", asked, want)
	}

	shown := map[string][]string{}
	for _, programme := range programmes {
		line := fmt.Sprintf("%s to %s: %s", programme.Start.In(brt).Format("Mon 15:04"), programme.Stop.In(brt).Format("Mon 15:04"), programme.Title)
		if programme.Description == unlisted {
			line += " (unlisted)"
		}
		shown[programme.ChannelID] = append(shown[programme.ChannelID], line)

		switch programme.Title {
		case "IURI & UDI":
			if want := "IURI & UDI - 14\nLorde Rodolfus reúne os vilões.\nE os espiões?"; programme.Description != want || programme.Image != "" {
				t.Errorf("%s: described as %q with the picture %q, want %q and none, for the site's own", programme.Title, programme.Description, programme.Image, want)
			}
		case "Roda Viva":
			if programme.Description != "CARLOS MELO" || programme.Image != poster {
				t.Errorf("%s: described as %q with the picture %q, want its episode and %q", programme.Title, programme.Description, programme.Image, poster)
			}
		}
	}

	want := []string{
		"Sun 23:30 to Mon 02:00: Cultura Livre", // on since the day before
		"Mon 02:00 to Mon 05:58: Balaio",
		"Mon 05:58 to Mon 06:02: ABERTURA DA EMISSORA",
		"Mon 06:02 to Mon 22:00: IURI & UDI",
		"Mon 22:00 to Tue 00:03: Roda Viva",
		// Persona ends on a day the guide lacks.
		"Tue 00:03 to Tue 01:00: TV Cultura (unlisted)",
	}
	if got := shown["tv-cultura"]; len(got) != len(want)+23 || !slices.Equal(got[:len(want)], want) {
		t.Errorf("TV Cultura's guide:\n%s\nwant, and then its name on every hour of Tuesday:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if got := shown["cultura-fast"]; len(got) != 48 {
		t.Errorf("Cultura Fast's guide: %q, want its name on every hour of the two days", got)
	}
}

func TestProgrammesRefused(t *testing.T) {
	tests := []struct {
		name string
		set  func(*cultura)
		want string // what the error must mention
	}{
		{"abroad", func(c *cultura) { c.status = http.StatusForbidden }, "-cultura-proxy"},
		{"down", func(c *cultura) { c.status = http.StatusBadGateway }, "502"},
		{"laid out anew", func(c *cultura) { c.days = map[string]string{"05102026": "<table><tr><td>06:02</td></tr></table>"} }, "nothing on TV Cultura"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c, p := serve(t)
			test.set(c)
			channels, _ := p.Channels(t.Context())
			from := time.Date(2026, 10, 5, 3, 0, 0, 0, time.UTC)

			_, err := p.Programmes(t.Context(), channels, from, from.Add(48*time.Hour))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Errorf("Programmes() error = %v, want one mentioning %q", err, test.want)
			}
		})
	}
}

func TestStream(t *testing.T) {
	c, p := serve(t)
	for _, media := range []string{
		head + segment,
		head + `#EXT-X-KEY:METHOD=AES-128,URI="key"` + "\n" + segment, // ffmpeg fetches the key
	} {
		c.media = media
		source, err := p.Stream(t.Context(), "cultura-fast")
		if want := (provider.Source{URL: p.channels[1].stream, Client: p.client}); err != nil || source != want {
			t.Errorf("Stream() = %+v, %v: want the master playlist and the provider's own HTTP client", source, err)
		}
	}
}

func TestStreamRefused(t *testing.T) {
	tests := []struct {
		name string
		set  func(*cultura)
		want string // what the error must mention
	}{
		{"gone", func(c *cultura) { c.status = http.StatusNotFound }, "unavailable"},
		{"not a playlist", func(c *cultura) { c.master = "<html>" }, "unavailable"},
		{"stopped", func(c *cultura) { c.media = head }, "unavailable"},
		{"sign-in", func(c *cultura) { c.status = http.StatusUnauthorized }, "sign-in"},
		{"abroad", func(c *cultura) { c.status = http.StatusForbidden }, "-cultura-proxy"},
		{"encrypted", func(c *cultura) {
			c.media = head + `#EXT-X-KEY:METHOD=SAMPLE-AES,URI="skd://key",KEYFORMAT="com.apple.streamingkeydelivery"` + "\n" + segment
		}, "DRM"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c, p := serve(t)
			test.set(c)

			_, err := p.Stream(t.Context(), "tv-cultura")
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Errorf("Stream() error = %v, want one mentioning %q", err, test.want)
			}
		})
	}
}
