package ebc

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
)

// The playlists of a channel, as EBC lays them out: the sound in a playlist of
// its own, and the lowest quality first.
const (
	master = `#EXTM3U
#EXT-X-VERSION:5
#EXT-X-MEDIA:TYPE=AUDIO,URI="EBC_HD-mp4a_128000=20000.m3u8",GROUP-ID="audio-AACL-128",LANGUAGE="und",NAME="Undetermined",DEFAULT=YES,AUTOSELECT=YES,CHANNELS="2"
#EXT-X-STREAM-INF:BANDWIDTH=1191608,AVERAGE-BANDWIDTH=1083279,CODECS="avc1.4d401e,mp4a.40.2",RESOLUTION=576x324,FRAME-RATE=29.970,AUDIO="audio-AACL-128"
EBC_HD-avc1_900000=10003.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=2824007,AVERAGE-BANDWIDTH=2567279,CODECS="avc1.4d401f,mp4a.40.2",RESOLUTION=1280x720,FRAME-RATE=29.970,AUDIO="audio-AACL-128"
EBC_HD-avc1_2300000=10000.m3u8
`
	head = `#EXTM3U
#EXT-X-VERSION:5
#EXT-X-TARGETDURATION:5
#EXT-X-MEDIA-SEQUENCE:96648236
#EXT-X-INDEPENDENT-SEGMENTS
`
	segment = `#EXT-X-PROGRAM-DATE-TIME:2026-10-05T11:27:02.618000Z
#EXTINF:4.004,
EBC_HD-avc1_2300000=10000-begin=3711292226180000-dur=40040000-seq=96648236.ts
`
)

// line is a line of a day's listing, as TV Brasil's site writes it.
func line(clock, programme string) string {
	return `        <div>
    <!-- Programação - Conteúdo linha -->
<div class="col-lg-1 col-md-1 col-sm-2 col-xs-3 horario">
	<span  class="date-display-single">` + clock + `</span></div>

<div class="col-lg-11 col-md-11 col-sm-10 col-xs-9 nomeprograma">
	` + programme + `</div>
  </div>
`
}

// ebc is a fake of EBC: TV Brasil's guide, and the playlists of a channel.
type ebc struct {
	days   map[string]string // the guide: the listing of a day, by its date
	asked  chan string       // the days of the guide asked for
	abroad bool              // the guide turns the address away, as EBC's sites do abroad
	status int               // if set, how the guide and the master playlist answer
	master string
	media  string // the playlist of the highest quality
}

// serve starts a fake EBC and returns it with a provider that talks to it:
// TV Brasil, which has a guide, and Canal Gov, which has none.
func serve(t *testing.T) (*ebc, *Provider) {
	t.Helper()
	e := &ebc{asked: make(chan string, 16), master: master, media: head + segment}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /programacao/{day}/2", func(w http.ResponseWriter, r *http.Request) {
		e.asked <- r.PathValue("day")
		listing, published := e.days[r.PathValue("day")]
		switch {
		case e.abroad:
			http.Redirect(w, r, "/templates/error/403.html", http.StatusMovedPermanently)
		case e.status != 0:
			w.WriteHeader(e.status)
		case !published:
			http.NotFound(w, r)
		default:
			io.WriteString(w, `<div class="view view-grade-horaria"><div class="view-content">`+listing+`</div></div>`)
		}
	})
	mux.HandleFunc("GET /templates/error/403.html", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "<html>403</html>")
	})
	mux.HandleFunc("GET /{channel}/index.m3u8", func(w http.ResponseWriter, r *http.Request) {
		if e.status != 0 {
			w.WriteHeader(e.status)
			return
		}
		io.WriteString(w, e.master)
	})
	mux.HandleFunc("GET /{channel}/EBC_HD-avc1_2300000=10000.m3u8", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, e.media)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	return e, &Provider{client: server.Client(), channels: []channel{
		{id: "tv-brasil", name: "TV Brasil", stream: server.URL + "/tvbrasil/index.m3u8", guide: server.URL + "/programacao"},
		{id: "canal-gov", name: "Canal Gov", stream: server.URL + "/canalgov/index.m3u8"},
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
	want := []string{"TV Brasil", "TV Brasil Internacional", "Canal Gov", "Canal Educação"}
	if !slices.Equal(names, want) {
		t.Errorf("Channels() = %q, want %q", names, want)
	}
}

func TestProgrammes(t *testing.T) {
	e, p := serve(t)
	e.days = map[string]string{
		"20261004": line("22:00", `<a href="/sessao-de-cinema">Sessão de Cinema</a>`),
		"20261005": line("01:30", `<a href="/um-milagre">Um Milagre</a>`) +
			line("03:30", "") +
			line("04:00", `<a href="/brasil-visto-de-cima">Brasil Visto de Cima</a>`) +
			line("23:30", `<a href="/semcensura">Sem Censura</a>`),
		"20261006": line("01:30", `<a href="/artes">Arte &amp; Cultura</a>`) +
			line("22:30", `<a href="/trilhadeletras">Trilha de Letras</a>`),
		// EBC has yet to publish the 7th.
	}

	// Two days from midnight in Brasília.
	from := time.Date(2026, 10, 5, 3, 0, 0, 0, time.UTC)
	channels, _ := p.Channels(t.Context())
	programmes, err := p.Programmes(t.Context(), channels, from, from.Add(48*time.Hour))
	if err != nil {
		t.Fatal(err)
	}

	// The days around the two, for the programmes that run over midnight.
	close(e.asked)
	var asked []string
	for day := range e.asked {
		asked = append(asked, day)
	}
	if want := []string{"20261004", "20261005", "20261006", "20261007", "20261008"}; !slices.Equal(asked, want) {
		t.Errorf("asked for the days %q, want %q", asked, want)
	}

	var brasil, gov []string
	for _, programme := range programmes {
		shown := fmt.Sprintf("%s to %s: %s", programme.Start.In(brt).Format("Mon 15:04"), programme.Stop.In(brt).Format("Mon 15:04"), programme.Title)
		switch programme.Description {
		case unnamed:
			shown += " (unnamed)"
		case unlisted:
			shown += " (unlisted)"
		}
		if programme.ChannelID == "tv-brasil" {
			brasil = append(brasil, shown)
		} else {
			gov = append(gov, shown)
		}
	}
	want := []string{
		"Sun 22:00 to Mon 01:30: Sessão de Cinema", // on since the day before
		"Mon 01:30 to Mon 03:30: Um Milagre",
		"Mon 03:30 to Mon 04:00: TV Brasil (unnamed)",
		"Mon 04:00 to Mon 23:30: Brasil Visto de Cima",
		"Mon 23:30 to Tue 01:30: Sem Censura",
		"Tue 01:30 to Tue 22:30: Arte & Cultura",
		// Trilha de Letras ends on a day the guide lacks.
		"Tue 22:30 to Tue 23:00: TV Brasil (unlisted)",
		"Tue 23:00 to Wed 00:00: TV Brasil (unlisted)",
	}
	if !slices.Equal(brasil, want) {
		t.Errorf("TV Brasil's guide:\n%s\nwant:\n%s", strings.Join(brasil, "\n"), strings.Join(want, "\n"))
	}
	if len(gov) != 48 || gov[0] != "Mon 00:00 to Mon 01:00: Canal Gov (unlisted)" || gov[47] != "Tue 23:00 to Wed 00:00: Canal Gov (unlisted)" {
		t.Errorf("Canal Gov's guide: %q, want its name on every hour of the two days", gov)
	}
}

func TestProgrammesRefused(t *testing.T) {
	tests := []struct {
		name string
		set  func(*ebc)
		want string // what the error must mention
	}{
		{"abroad", func(e *ebc) { e.abroad = true }, "-ebc-proxy"},
		{"down", func(e *ebc) { e.status = http.StatusBadGateway }, "502"},
		{"laid out anew", func(e *ebc) { e.days = map[string]string{"20261005": "<table><tr><td>01:30</td></tr></table>"} }, "nothing on TV Brasil"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			e, p := serve(t)
			test.set(e)
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
	e, p := serve(t)
	for _, media := range []string{
		head + segment,
		head + `#EXT-X-KEY:METHOD=AES-128,URI="key"` + "\n" + segment, // ffmpeg fetches the key
	} {
		e.media = media
		source, err := p.Stream(t.Context(), "tv-brasil")
		if err != nil {
			t.Fatal(err)
		}
		if source.URL != p.channels[0].stream || source.Client != p.client {
			t.Errorf("Stream() = %+v, want the master playlist and the provider's own HTTP client", source)
		}
	}
}

func TestStreamRefused(t *testing.T) {
	tests := []struct {
		name string
		set  func(*ebc)
		want string // what the error must mention
	}{
		{"gone", func(e *ebc) { e.status = http.StatusNotFound }, "unavailable"},
		{"not a playlist", func(e *ebc) { e.master = "<html>" }, "unavailable"},
		{"stopped", func(e *ebc) { e.media = head }, "unavailable"},
		{"sign-in", func(e *ebc) { e.status = http.StatusUnauthorized }, "sign-in"},
		{"abroad", func(e *ebc) { e.status = http.StatusForbidden }, "-ebc-proxy"},
		{"encrypted", func(e *ebc) {
			e.media = head + `#EXT-X-KEY:METHOD=SAMPLE-AES,URI="skd://key",KEYFORMAT="com.apple.streamingkeydelivery"` + "\n" + segment
		}, "DRM"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			e, p := serve(t)
			test.set(e)

			_, err := p.Stream(t.Context(), "canal-gov")
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Errorf("Stream() error = %v, want one mentioning %q", err, test.want)
			}
		})
	}
}
