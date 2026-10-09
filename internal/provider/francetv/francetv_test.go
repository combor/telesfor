package francetv

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/combor/telesfor/internal/provider"
	"github.com/combor/telesfor/internal/provider/providertest"
)

// The playlists of a channel, as France Télévisions lays them out: six
// qualities in the real one, the sound in playlists of its own, and a key
// that half of the channels name by its full address.
const (
	master = `#EXTM3U
#EXT-X-VERSION:5
#EXT-X-MEDIA:TYPE=AUDIO,URI="described.m3u8",GROUP-ID="audio",LANGUAGE="qad",NAME="Audio Description",AUTOSELECT=YES
#EXT-X-MEDIA:TYPE=AUDIO,URI="french.m3u8",GROUP-ID="audio",LANGUAGE="fr",NAME="Francais",DEFAULT=YES,AUTOSELECT=YES
#EXT-X-MEDIA:TYPE=SUBTITLES,URI="subtitles.m3u8",GROUP-ID="text",LANGUAGE="fr",NAME="Français",DEFAULT=YES
#EXT-X-STREAM-INF:BANDWIDTH=255244,AVERAGE-BANDWIDTH=9999999,RESOLUTION=256x144,AUDIO="audio",SUBTITLES="text"
low.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=5947655,AVERAGE-BANDWIDTH=5406959,RESOLUTION=1920x1080,AUDIO="audio",SUBTITLES="text"
high.m3u8
#EXT-X-I-FRAME-STREAM-INF:BANDWIDTH=58976,URI="frames.m3u8"
`
	head = `#EXTM3U
#EXT-X-VERSION:5
#EXT-X-TARGETDURATION:8
#EXT-X-MEDIA-SEQUENCE:48337899
`
	segment = `#EXTINF:7.68,
high-48337899.ts
`
)

// listing is an entry of the guide, as france.tv writes it. One that is no
// episode of a programme has neither slug nor programme.
func listing(id int, start, length, title, page, slug, programme string) map[string]any {
	tracking := map[string]any{"program": nil, "program_id": nil}
	if slug != "" {
		tracking = map[string]any{"program": slug, "program_id": programme}
	}
	return map[string]any{
		"content": map[string]any{
			"id": id, "title": title, "description": "About " + title + ".", "url": page,
			"isoDate": "2026-10-" + start + "+02:00", "isoDuration": length,
			"images": map[string]any{"base": map[string]string{"x1": "https://medias.example/small.webp"}},
		},
		"tracking": tracking,
	}
}

// france is a fake of France Télévisions: the guide, the apps' API, the
// player's API and the servers that hand the streams out.
type france struct {
	url string

	mu     sync.Mutex
	days   map[string][]map[string]any // the guide of France 2, by day
	shows  map[string]string           // what the apps' API has, by path
	asked  []string                    // the days of the guide and the paths asked for
	status int                         // if set, how the guide and the apps' API answer
	reason string                      // and the reason that comes with it
	delay  time.Duration               // how long the apps' API takes over an answer

	video    string          // what the player's API says of France 2's stream
	refused  int             // if set, how the player's API answers instead
	passes   int             // how many passes have been handed out
	revoked  map[string]bool // the passes that are good no more
	index    string          // the playlist a stream is handed out at: a master, mostly
	media    string          // the playlist of a quality
	blocked  map[string]int  // how the servers answer for a file, if not with it
	ranged   map[string]bool // the files asked for by range, with a good pass
	fetched  map[string]int  // how often each file was asked for
	directed int             // how often the list of live channels was asked for
}

// serve starts a fake France Télévisions and returns it with a provider that
// talks to it.
func serve(t *testing.T) (*france, *Provider) {
	t.Helper()
	f := &france{index: master, revoked: map[string]bool{}, blocked: map[string]int{}, ranged: map[string]bool{}, fetched: map[string]int{}}
	answer := func(w http.ResponseWriter, status int) bool {
		if status == 0 {
			return false
		}
		w.Header().Set("X-ErrorType", f.reason)
		w.WriteHeader(status)
		return true
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/epg/videos/", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		day := r.URL.Query().Get("date")
		f.asked = append(f.asked, r.URL.Query().Get("channel")+" "+day)
		if !answer(w, f.status) {
			json.NewEncoder(w).Encode(append([]map[string]any{}, f.days[day]...))
		}
	})
	mux.HandleFunc("GET /generic/taxonomy/{path}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		delay := f.delay
		f.mu.Unlock()
		time.Sleep(delay)
		f.mu.Lock()
		defer f.mu.Unlock()
		f.asked = append(f.asked, r.PathValue("path"))
		show, ok := f.shows[r.PathValue("path")]
		switch {
		case answer(w, f.status):
		case !ok:
			http.Error(w, `{"error":"Taxonomy not found!"}`, http.StatusNotFound)
		default:
			io.WriteString(w, show)
		}
	})
	mux.HandleFunc("GET /generic/directs", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.directed++
		io.WriteString(w, `{"items":[{"channel":null},{"channel":{"channel_path":"france-2","si_id":"live-2"}}]}`)
	})
	mux.HandleFunc("GET /videos/live-2", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.URL.Query().Get("domain") == "" {
			w.WriteHeader(http.StatusBadRequest)
		} else if f.refused != 0 {
			w.WriteHeader(f.refused)
			io.WriteString(w, `{"code":2012,"message":"Cette vidéo n'est pas disponible."}`)
		} else {
			io.WriteString(w, f.video)
		}
	})
	mux.HandleFunc("GET /sign", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.passes++
		stream := strings.TrimPrefix(r.URL.Query().Get("url"), f.url)
		json.NewEncoder(w).Encode(map[string]string{"url": fmt.Sprintf("%s/pass%d%s?hdnea=short", f.url, f.passes, stream)})
	})
	mux.HandleFunc("/{pass}/live/france-2/{file}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		file := r.PathValue("file")
		f.fetched[file]++
		if f.revoked[r.PathValue("pass")] {
			w.Header().Set("X-ErrorType", "ltoken")
			w.WriteHeader(http.StatusForbidden)
			return
		}
		f.ranged[file] = f.ranged[file] || r.Header.Get("Range") != ""
		switch {
		case answer(w, f.blocked[file]):
		case file == "index.m3u8":
			io.WriteString(w, f.index)
		case strings.HasSuffix(file, ".m3u8"):
			io.WriteString(w, f.media)
		default:
			io.WriteString(w, "picture")
		}
	})
	mux.HandleFunc("GET /keys/hls.key", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "0123456789abcdef") })
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	f.url = server.URL
	f.video = `{"video":{"url":"` + f.url + `/live/france-2/index.m3u8","token":{"akamai":"` + f.url + `/sign?format=json"},"drm":false,"format":"hls"}}`
	f.media = head + `#EXT-X-KEY:METHOD=AES-128,URI="` + f.url + `/keys/hls.key"` + "\n" + segment

	p, err := New("")
	if err != nil {
		t.Fatal(err)
	}
	p.client, p.rest = server.Client(), 0
	p.guide, p.apps, p.player = f.url+"/api/epg/videos/", f.url+"/generic", f.url+"/videos"
	return f, p
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
		if ch.ID == "" || ch.Logo == "" || ch.Place != i+1 {
			t.Errorf("channel %+v, want an id, a logo and place %d", ch, i+1)
		}
	}
	if want := []string{"France 2", "France 3", "France 4", "France 5", "franceinfo"}; !slices.Equal(names, want) {
		t.Errorf("Channels() = %q, want %q", names, want)
	}
}

func TestProgrammes(t *testing.T) {
	const poster = "https://medias.example/soeurs-800.jpg"
	f, p := serve(t)
	f.days = map[string][]map[string]any{
		"2026-10-06": {
			listing(1, "06T21:10:08", "PT1H30M56S", "Le drame de Crépol", "/france-2/apres-la-colere/1-le-drame-de-crepol.html", "apres_la_colere", "100185"),
			// Without a page, and under two names.
			listing(2, "06T22:45:00", "PT1H5M0S", "Audience à Poitiers", "", "justice_en_france|justice_en_fr", "40144"),
		},
		"2026-10-07": {
			// Nothing is listed for the night. The apps' API does not have
			// this one where its name would put it.
			listing(3, "07T06:00:00", "PT0H35M0S", "Émission du mercredi 7 octobre 2026", "", "dans_le_retro", "97749"),
			listing(4, "07T06:35:00", "PT0H16M0S", "Emission du 7 octobre 2026", "/enfants/six-huit-ans/okoo-koo/saison-7/4-emission.html", "okoo_koo", "38221"),
			// The episode has no name of its own.
			listing(5, "07T06:50:00", "PT0H5M0S", "Journal Météo climat", "", "journal_meteo_climat", "2371"),
			listing(6, "07T21:10:00", "PT0H50M0S", "S1 E3 - L'autre vie d'Elodie", "/france-2/soeurs/soeurs-saison-1/6-l-autre-vie-d-elodie.html", "surs", "100038"),
			// Listed twice, as it starts before the slot of the one before
			// is over.
			listing(7, "07T21:58:00", "PT0H46M34S", "S1 E4 - Le piège", "", "surs", "100038"),
			listing(7, "07T22:00:00", "PT0H46M34S", "S1 E4 - Le piège", "", "surs", "100038"),
			listing(8, "07T22:50:00", "PT1H32M0S", "Un père idéal", "/films/8-un-pere-ideal.html", "", ""),
		},
	}
	f.shows = map[string]string{
		"france-2_apres-la-colere":      `{"id":100185,"label":"Après la colère"}`,
		"france-2_justice-en-france":    `{"id":40144,"label":"Justice en France"}`,
		"enfants_six-huit-ans":          `{"id":5,"label":"6-8 ans"}`,
		"enfants_six-huit-ans_okoo-koo": `{"id":38221,"label":"Okoo-koo"}`,
		"france-2_journal-meteo-climat": `{"id":2371,"label":"Journal Météo Climat"}`,
		"france-2_soeurs": `{"id":100038,"label":"Sœurs","images":[{"type":"vignette_16x9","urls":{"w:800":"https://medias.example/wide.jpg"}},
			{"type":"vignette_3x4","urls":{"w:400":"https://medias.example/soeurs-400.jpg","w:800":"` + poster + `"}}]}`,
	}

	// Two days from ten in the evening in Paris.
	from := time.Date(2026, 10, 6, 20, 0, 0, 0, time.UTC)
	channels, _ := p.Channels(t.Context())
	programmes, err := p.Programmes(t.Context(), channels[:1], from, from.Add(48*time.Hour))
	if err != nil {
		t.Fatal(err)
	}

	// The day before the three as well, for what runs over midnight. A
	// programme is looked for under the address of its page, from the
	// second part on, then under its name, until it is found.
	slices.Sort(f.asked)
	want := []string{
		"enfants_six-huit-ans", "enfants_six-huit-ans_okoo-koo",
		"france-2 2026-10-05", "france-2 2026-10-06", "france-2 2026-10-07", "france-2 2026-10-08",
		"france-2_apres-la-colere", "france-2_dans-le-retro", "france-2_journal-meteo-climat", "france-2_justice-en-france", "france-2_soeurs",
	}
	if !slices.Equal(f.asked, want) {
		t.Errorf("asked for\n%q, want\n%q", f.asked, want)
	}

	paris := time.FixedZone("CEST", 2*60*60)
	var shown []string
	for _, programme := range programmes {
		if programme.Title == "France 2" && programme.Description == unlisted {
			continue
		}
		shown = append(shown, fmt.Sprintf("%s to %s: %s", programme.Start.In(paris).Format("Mon 15:04"), programme.Stop.In(paris).Format("Mon 15:04"), programme.Title))

		switch programme.Title {
		case "Sœurs":
			if !strings.HasPrefix(programme.Description, "S1 E") || !strings.HasSuffix(programme.Description, ".") || programme.Image != poster {
				t.Errorf("%s: described as %q with the picture %q, want its episode first and %q", programme.Title, programme.Description, programme.Image, poster)
			}
		case "Journal Météo Climat":
			if programme.Description != "About Journal Météo climat." {
				t.Errorf("%s: described as %q, want no episode of the same name", programme.Title, programme.Description)
			}
		case "Un père idéal":
			if programme.Description != "About Un père idéal." || programme.Image != "https://medias.example/small.webp" {
				t.Errorf("%s: described as %q with the picture %q, want its own", programme.Title, programme.Description, programme.Image)
			}
		}
	}
	listed := []string{
		"Tue 21:10 to Tue 22:45: Après la colère", // on since before, and until the next starts
		"Tue 22:45 to Tue 23:50: Justice en France",
		"Wed 06:00 to Wed 06:35: Dans le retro",
		"Wed 06:35 to Wed 06:50: Okoo-koo", // cut short by the next
		"Wed 06:50 to Wed 06:55: Journal Météo Climat",
		"Wed 21:10 to Wed 21:58: Sœurs",
		"Wed 21:58 to Wed 22:50: Sœurs",
		"Wed 22:50 to Thu 00:22: Un père idéal",
	}
	if len(programmes) != 53 || !slices.Equal(shown, listed) {
		t.Errorf("France 2's guide of %d programmes, %d of them its name:\n%s", len(programmes), len(programmes)-len(shown), strings.Join(shown, "\n"))
	}

	// A programme is looked up once, found or not.
	if _, err := p.Programmes(t.Context(), channels[:1], from, from.Add(48*time.Hour)); err != nil || len(f.asked) != len(want)+4 {
		t.Errorf("the guide again: %v, having asked %d times in all, want the four days and no programme again", err, len(f.asked))
	}
}

// Guides asked for at once all come with the names of the programmes: one
// waits for what another is looking up.
func TestProgrammesAtOnce(t *testing.T) {
	f, p := serve(t)
	f.days = map[string][]map[string]any{"2026-10-07": {listing(6, "07T21:10:00", "PT0H50M0S", "S1 E3 - L'autre vie d'Elodie", "", "surs", "100038")}}
	f.shows = map[string]string{"france-2_surs": `{"id":100038,"label":"Sœurs"}`}
	f.delay = 50 * time.Millisecond
	channels, _ := p.Channels(t.Context())
	from := time.Date(2026, 10, 6, 20, 0, 0, 0, time.UTC)

	var asking sync.WaitGroup
	for range 4 {
		asking.Go(func() {
			programmes, err := p.Programmes(t.Context(), channels[:1], from, from.Add(48*time.Hour))
			if err != nil || !slices.ContainsFunc(programmes, func(programme provider.Programme) bool { return programme.Title == "Sœurs" }) {
				t.Errorf("Programmes() = %v, without Sœurs by its name", err)
			}
		})
	}
	asking.Wait()
}

func TestProgrammesRefused(t *testing.T) {
	tests := []struct {
		name string
		set  func(*france)
		want string // what the error must mention
	}{
		{"abroad", func(f *france) { f.status, f.reason = http.StatusForbidden, "geo" }, "-francetv-proxy"},
		{"down", func(f *france) { f.status = http.StatusBadGateway }, "502"},
		{"laid out anew", func(f *france) { f.days = nil }, "nothing on France 2"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			f, p := serve(t)
			f.days = map[string][]map[string]any{"2026-10-07": {listing(1, "07T06:00:00", "PT1H0M0S", "Le 6h info", "", "", "")}}
			test.set(f)
			channels, _ := p.Channels(t.Context())
			from := time.Date(2026, 10, 6, 20, 0, 0, 0, time.UTC)

			_, err := p.Programmes(t.Context(), channels[:1], from, from.Add(48*time.Hour))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Errorf("Programmes() error = %v, want one mentioning %q", err, test.want)
			}
		})
	}
}

func TestStream(t *testing.T) {
	f, p := serve(t)
	source, err := p.Stream(t.Context(), "france-2")
	if want := f.url + "/pass1/live/france-2/index.m3u8?hdnea=short"; err != nil || source.URL != want {
		t.Fatalf("Stream() = %+v, %v: want the master playlist at %s", source, err, want)
	}
	// As ffmpeg fetches: a range of everything.
	fetch := func(file string) (int, string) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, f.url+"/pass1/live/france-2/"+file, nil)
		req.Header.Set("Range", "bytes=0-")
		resp, err := source.Client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}

	// Every quality, for the remuxer to choose from.
	if status, playlist := fetch("index.m3u8?hdnea=short"); status != http.StatusOK || playlist != master || f.fetched["index.m3u8"] != 2 {
		t.Errorf("the master playlist: %d after %d fetches\n%s\nwant\n%s", status, f.fetched["index.m3u8"], playlist, master)
	}
	// The key by its path, for ffmpeg to ask the relay for it.
	want := head + `#EXT-X-KEY:METHOD=AES-128,URI="/keys/hls.key"` + "\n" + segment
	if status, playlist := fetch("high.m3u8"); status != http.StatusOK || playlist != want || f.ranged["high.m3u8"] {
		t.Errorf("the playlist of the quality: %d, asked for by range: %t\n%s\nwant it whole:\n%s", status, f.ranged["high.m3u8"], playlist, want)
	}
	// The pass runs out: the stream goes on with a new one.
	f.revoked["pass1"] = true
	if status, picture := fetch("high-48337899.ts"); status != http.StatusOK || picture != "picture" || f.passes != 2 || !f.ranged["high-48337899.ts"] {
		t.Errorf("a segment after the pass ran out: %d %q with %d passes handed out, asked for by range: %t, want it with a second pass, by range",
			status, picture, f.passes, f.ranged["high-48337899.ts"])
	}
	f.revoked["pass2"] = true
	if status, _ := fetch("high.m3u8"); status != http.StatusOK || f.passes != 3 {
		t.Errorf("a playlist after the second pass ran out: %d with %d passes handed out, want 200 with a third", status, f.passes)
	}
	// A new pass that is refused is refused for something else.
	p.rest = time.Hour
	again, err := p.Stream(t.Context(), "france-2")
	if err != nil {
		t.Fatal(err)
	}
	f.revoked["pass4"] = true
	resp, _ := providertest.Get(t, again.Client, f.url+"/pass4/live/france-2/high.m3u8")
	if resp.StatusCode != http.StatusForbidden || f.passes != 4 {
		t.Errorf("a new pass refused: %s with %d passes handed out, want 403 and no pass more", resp.Status, f.passes)
	}
	if f.directed != 1 {
		t.Errorf("asked for the list of live channels %d times, want once for both tunes", f.directed)
	}
}

// A stream of one quality is its playlist as it stands: ffmpeg reads it anew
// as the stream goes on.
func TestStreamOfOneQuality(t *testing.T) {
	f, p := serve(t)
	f.index = head + segment
	source, err := p.Stream(t.Context(), "france-2")
	if err != nil {
		t.Fatal(err)
	}
	later := head + segment + "#EXTINF:7.68,\nhigh-48337900.ts\n"
	f.mu.Lock()
	f.index = later
	f.mu.Unlock()

	if _, playlist := providertest.Get(t, source.Client, source.URL); playlist != later {
		t.Errorf("the playlist, read again:\n%s\nwant the one with the next segment:\n%s", playlist, later)
	}
}

func TestStreamRefused(t *testing.T) {
	tests := []struct {
		name string
		set  func(*france)
		want string // what the error must mention
	}{
		{"abroad", func(f *france) { f.blocked["index.m3u8"], f.reason = http.StatusForbidden, "geo" }, "-francetv-proxy"},
		// The playlists are handed out anywhere.
		{"picture abroad", func(f *france) { f.blocked["high-48337899.ts"], f.reason = http.StatusForbidden, "geo" }, "-francetv-proxy"},
		{"turned away", func(f *france) { f.blocked["index.m3u8"], f.reason = http.StatusForbidden, "abuse" }, "refused here"},
		{"gone", func(f *france) { f.blocked["index.m3u8"] = http.StatusNotFound }, "unavailable"},
		{"stopped", func(f *france) { f.media = head }, "unavailable"},
		{"no video", func(f *france) { f.refused = http.StatusNotFound }, "Cette vidéo n'est pas disponible."},
		{"encrypted", func(f *france) { f.video = strings.Replace(f.video, `"drm":false`, `"drm":true`, 1) }, "DRM"},
		{"encrypted all the same", func(f *france) {
			f.media = head + `#EXT-X-KEY:METHOD=SAMPLE-AES,URI="skd://key",KEYFORMAT="com.apple.streamingkeydelivery"` + "\n" + segment
		}, "DRM"},
		{"in another format", func(f *france) { f.video = strings.Replace(f.video, `"hls"`, `"dash"`, 1) }, "no HLS stream"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			f, p := serve(t)
			test.set(f)

			_, err := p.Stream(t.Context(), "france-2")
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Errorf("Stream() error = %v, want one mentioning %q", err, test.want)
			}
		})
	}

	// The fake has no other channel: France Télévisions lists none live.
	_, p := serve(t)
	if _, err := p.Stream(t.Context(), "france-3"); err == nil || !strings.Contains(err.Error(), "no live stream") {
		t.Errorf("Stream() of a channel that is not listed: %v, want an error that says so", err)
	}
	if source, err := p.Stream(t.Context(), "arte"); err == nil {
		t.Errorf("Stream() of a channel that is not offered = %+v, want an error", source)
	}
}
