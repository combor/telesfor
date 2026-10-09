package wppilot

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/combor/telesfor/internal/httpclient"
	"github.com/combor/telesfor/internal/provider"
	"github.com/combor/telesfor/internal/provider/providertest"
	"github.com/combor/telesfor/internal/store"
)

// catalogue is WP Pilot's list of channels: in its own order, with one that
// needs a package, one that TVP's own tuner has, and a radio station.
const catalogue = `[
	{"id": 158, "name": "Telewizja WP HD", "access_status": "free", "is_audio_only": false, "icon": {"dark": "https://img.example/158.png"}},
	{"id": 16, "name": "TVN24", "access_status": "unsubscribed", "is_audio_only": false, "icon": {"dark": "https://img.example/16.png"}},
	{"id": 3, "name": "TVP 1 HD", "access_status": "free", "is_audio_only": false, "icon": {"dark": "https://img.example/3.png"}},
	{"id": 12, "name": "TV Puls HD", "access_status": "free", "is_audio_only": false, "icon": {"dark": ""}},
	{"id": 240, "name": "Radio 357", "access_status": "free", "is_audio_only": true, "icon": {"dark": "https://img.example/240.png"}},
	{"id": 9, "name": "Polsat HD", "access_status": "free", "is_audio_only": false, "icon": {"dark": "https://img.example/9.png"}}
]`

// lineup is what the provider makes of the catalogue.
var lineup = []channel{
	{ID: 9, Name: "Polsat HD", Logo: "https://img.example/9.png"},
	{ID: 12, Name: "TV Puls HD"},
	{ID: 158, Name: "Telewizja WP HD", Logo: "https://img.example/158.png"},
}

// channels is the lineup as the tuner gets it.
var channels = []provider.Channel{
	{ID: "9", Name: "Polsat HD", Logo: "https://img.example/9.png", Place: 9},
	{ID: "12", Name: "TV Puls HD", Place: 12},
	{ID: "158", Name: "Telewizja WP HD", Logo: "https://img.example/158.png", Place: 158},
}

// pilot is a fake of WP Pilot: of its API, as much as the provider uses, and
// of a stream server.
type pilot struct {
	url string

	mu       sync.Mutex
	held     chan struct{} // if set, a request for a code waits for it to close
	holding  chan struct{} // closed when the first such request waits
	code     string        // how WP answers a question about the code: with this refusal, or the session once "entered"
	consents bool          // the account has yet to accept them
	val      string        // the session's second cookie, as WP wants it now
	reissue  string        // if set, the next answer to the session sets that cookie to this
	revoked  bool          // WP no longer knows the session

	refusal   string        // of an open, by WP's name for it
	licensed  map[int]bool  // channels WP names licence servers for
	encrypted map[int]bool  // channels whose playlists say so
	dashOnly  map[int]bool  // channels without an HLS stream
	shut      map[int]bool  // channels whose playlists are refused
	waiting   chan struct{} // if set, an open waits for it to close
	opening   chan struct{} // closed when the first such open waits
	crossed   func()        // if set, what another call does while the next open is on its way
	slots     int           // how many streams WP plays the account at once; zero for any number
	live      int           // how many it plays now
	full      chan struct{} // if set, closed when an open is first refused for the account's limit
	shutting  chan struct{} // if set, a close waits for it to close
	pulse     string        // where a stream's signs of life go, if not to the API

	opened  []string // the opens: who asked, for what
	beats   int
	closed  []string // the tokens of the sessions closed
	guide   map[int]string
	guides  []string // the guide requests: the channels asked about, and the cookies sent
	fetched []string // the requests to the stream server: who asked, with which cookies
}

// set changes how the fake answers.
func (f *pilot) set(change func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	change()
}

// see reads what the fake has noted.
func see[T any](f *pilot, read func() T) T {
	f.mu.Lock()
	defer f.mu.Unlock()
	return read()
}

func refuse(w http.ResponseWriter, status int, name, info string) {
	w.WriteHeader(status)
	fmt.Fprintf(w, `{"data": null, "_meta": {"error": {"name": %q, "code": 1, "info": %s}}}`, name, info)
}

// serve starts a fake WP Pilot and returns it with a provider that talks to
// it and keeps its sign-in in db.
func serve(t *testing.T, db *bolt.DB) (*pilot, *Provider) {
	t.Helper()
	f := &pilot{code: "code_need_verify", val: "val1", guide: map[int]string{}}
	// known tells whether a request comes with the session, and reissues its
	// cookie if that is due.
	known := func(w http.ResponseWriter, r *http.Request) bool {
		id, _ := r.Cookie(sessionID)
		val, _ := r.Cookie(sessionVal)
		if f.revoked || id == nil || val == nil || id.Value != "sid" || val.Value != f.val {
			return false
		}
		if f.reissue != "" {
			f.val, f.reissue = f.reissue, ""
			http.SetCookie(w, &http.Cookie{Name: sessionVal, Value: f.val})
		}
		return true
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/user_auth/activate_code", func(w http.ResponseWriter, r *http.Request) {
		if held := see(f, func() chan struct{} { return f.held }); held != nil {
			close(f.holding)
			<-held
		}
		fmt.Fprintf(w, `{"data": {"code": "ABC123", "url": "%s/kod/"}, "_meta": null}`, r.Host)
	})
	mux.HandleFunc("POST /api/v1/user_auth/verify_code", func(w http.ResponseWriter, r *http.Request) {
		if body, _ := io.ReadAll(r.Body); !strings.Contains(string(body), `"ABC123"`) {
			t.Errorf("asked about the code with %s", body)
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.code != "entered" {
			refuse(w, http.StatusUnprocessableEntity, f.code, "null")
			return
		}
		http.SetCookie(w, &http.Cookie{Name: sessionID, Value: "sid"})
		http.SetCookie(w, &http.Cookie{Name: sessionVal, Value: f.val})
		fmt.Fprintf(w, `{"data": {"type": "free", "needs_gdpr": %t}, "_meta": null}`, f.consents)
	})
	mux.HandleFunc("GET /api/v3/channels/list", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if !known(w, r) {
			refuse(w, http.StatusForbidden, "not_authorized", "null")
			return
		}
		io.WriteString(w, `{"data": `+catalogue+`, "_meta": null}`)
	})
	mux.HandleFunc("GET /api/v2/user", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if !known(w, r) {
			// As to anyone WP does not know: a guest's session in place of
			// the one that came, and no user.
			http.SetCookie(w, &http.Cookie{Name: sessionID, Value: "g:guest"})
			http.SetCookie(w, &http.Cookie{Name: sessionVal, MaxAge: -1})
			io.WriteString(w, `{"data": null, "_meta": null}`)
			return
		}
		fmt.Fprintf(w, `{"data": {"type": "free", "needs_gdpr": %t}, "_meta": null}`, f.consents)
	})
	mux.HandleFunc("GET /api/v3/channel/{id}", func(w http.ResponseWriter, r *http.Request) {
		if waiting := see(f, func() chan struct{} { return f.waiting }); waiting != nil {
			close(f.opening)
			<-waiting
		}
		if crossed := see(f, func() func() { defer func() { f.crossed = nil }(); return f.crossed }); crossed != nil {
			crossed()
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		id, _ := strconv.Atoi(r.PathValue("id"))
		f.opened = append(f.opened, fmt.Sprintf("%d as %s from %s by %s", id, r.URL.Query().Get("device_type"), r.Referer(), r.UserAgent()))
		if f.slots > 0 && f.live >= f.slots && f.full != nil {
			close(f.full)
			f.full = nil
		}
		switch {
		case !known(w, r):
			refuse(w, http.StatusForbidden, "not_authorized", "null")
			return
		case f.refusal == "multiroom_limit_exceeded" || f.slots > 0 && f.live >= f.slots:
			refuse(w, http.StatusUnprocessableEntity, "multiroom_limit_exceeded", `{"reason": "global_limit_exceeded", "streams": [
				{"channel_id": "9", "channel_name": "Polsat HD", "user_ip": "192.0.2.1"},
				{"channel_id": "14", "channel_name": "TVN", "user_ip": "192.0.2.1"}]}`)
			return
		case f.refusal == "channel_switch_limit":
			refuse(w, http.StatusUnprocessableEntity, f.refusal, `["of another shape"]`)
			return
		case f.refusal != "":
			refuse(w, http.StatusUnprocessableEntity, f.refusal, "null")
			return
		}
		token := "tok" + strconv.Itoa(len(f.opened))
		f.live++
		streams := fmt.Sprintf(`{"type": "dash@live:abr", "url": ["%s/cdn/%d/manifest.mpd?t=%s"]}`, f.url, id, token)
		if !f.dashOnly[id] {
			streams += fmt.Sprintf(`, {"type": "hls@live:abr", "url": ["%s/cdn/%d/playlist.m3u8?t=%s", "http://other.example/%d/playlist.m3u8"]}`, f.url, id, token, id)
		}
		drms := "null"
		if f.licensed[id] {
			drms = `{"widevine": "/api/v1/drm/widevine?t=1", "fairplay": "/api/v1/drm/fairplay?t=1"}`
		}
		pulse := f.url + "/api/v1/heartbeat?t=" + token
		if f.pulse != "" {
			pulse = f.pulse
		}
		fmt.Fprintf(w, `{"data": {"token": %q, "heartbeat": {"interval": 0.005, "url": %q},
			"stream_channel": {"channel_name": "x", "streams": [%s], "drms": %s}},
			"_meta": {"switches": {"initial_counter": 250, "left": 249, "can_switch_since": 0}}}`, token, pulse, streams, drms)
	})
	mux.HandleFunc("POST /api/v1/heartbeat", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if !known(w, r) || r.URL.Query().Get("t") == "" {
			refuse(w, http.StatusForbidden, "not_authorized", "null")
			return
		}
		f.beats++
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v2/channels/close", func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Token string }
		json.NewDecoder(r.Body).Decode(&body)
		if shutting := see(f, func() chan struct{} { return f.shutting }); shutting != nil {
			<-shutting
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if !known(w, r) {
			refuse(w, http.StatusForbidden, "not_authorized", "null")
			return
		}
		f.live--
		f.closed = append(f.closed, body.Token)
		io.WriteString(w, `{"data": {"status": "ok"}, "_meta": null}`)
	})
	mux.HandleFunc("GET /api/v2/epg", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		asked := r.URL.Query().Get("channels")
		f.guides = append(f.guides, asked+" with "+r.Header.Get("Cookie"))
		var data []string
		for id := range strings.SplitSeq(asked, ",") {
			n, _ := strconv.Atoi(id)
			data = append(data, fmt.Sprintf(`{"channel_id": %d, "entries": [%s]}`, n, f.guide[n]))
		}
		io.WriteString(w, `{"data": [`+strings.Join(data, ",")+`], "_meta": null}`)
	})
	mux.HandleFunc("GET /cdn/{id}/{file}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		id, _ := strconv.Atoi(r.PathValue("id"))
		f.fetched = append(f.fetched, fmt.Sprintf("%s by %s with %s", r.PathValue("file"), r.UserAgent(), r.Header.Get("Cookie")))
		switch file := r.PathValue("file"); {
		case f.shut[id] || r.UserAgent() != browser:
			w.WriteHeader(http.StatusForbidden)
		case file == "playlist.m3u8":
			io.WriteString(w, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1824000\nchunklist.m3u8?t=1\n")
		case file == "chunklist.m3u8" && f.encrypted[id]:
			io.WriteString(w, "#EXTM3U\n#EXT-X-KEY:METHOD=SAMPLE-AES,URI=\"skd://12\"\n#EXTINF:5.0,\nmedia.ts\n")
		default:
			io.WriteString(w, "#EXTM3U\n#EXTINF:5.0,\nmedia.ts\n")
		}
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	f.url = server.URL

	client := *server.Client()
	client.Transport = httpclient.Wrap(client.Transport, agent)
	p := &Provider{client: &client, db: db, site: server.URL, poll: time.Millisecond, codeLife: time.Minute}
	var err error
	if p.account, err = load(db); err != nil {
		t.Fatal(err)
	}
	return f, p
}

// signedIn returns a fake WP Pilot and a provider that has an account with it.
func signedIn(t *testing.T) (*pilot, *Provider) {
	t.Helper()
	f, p := serve(t, nil)
	p.account = &account{ID: "sid", Val: "val1", Channels: slices.Clone(lineup)}
	return f, p
}

// viewers counts those the provider takes to be watching.
func viewers(p *Provider) (n int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, watched := range p.viewings {
		n += watched.viewers
	}
	return n
}

// closes waits for WP to have been asked to close n sessions, and returns
// those it was asked to.
func closes(f *pilot, n int) []string {
	for range 2000 {
		if see(f, func() int { return len(f.closed) }) >= n {
			break
		}
		time.Sleep(time.Millisecond)
	}
	return see(f, func() []string { return slices.Clone(f.closed) })
}

// tune opens a stream as a viewer does, and returns it with what the viewer
// does to leave: that waits for the provider to take note, and for the
// sessions to be closed if this was the last viewer of all.
func tune(t *testing.T, p *Provider, channelID string) (provider.Source, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	source, err := p.Stream(ctx, channelID)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	return source, func() {
		t.Helper()
		stay := viewers(p) - 1
		cancel()
		for range 2000 {
			if viewers(p) == stay {
				break
			}
			time.Sleep(time.Millisecond)
		}
		if viewers(p) != stay {
			t.Fatalf("%d are taken to be watching after one of them left, want %d", viewers(p), stay)
		}
		if stay == 0 {
			p.watching.Wait()
		}
	}
}

func TestSignIn(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	f, p := serve(t, db)
	changes := make(chan struct{}, 2)
	p.OnChange(func() { changes <- struct{}{} })

	if got, _ := p.Channels(t.Context()); len(got) != 0 || p.Login() != (provider.Login{}) {
		t.Fatalf("before signing in: channels %v, sign-in %+v", got, p.Login())
	}
	if err := p.SignIn(t.Context()); err != nil {
		t.Fatal(err)
	}
	login := p.Login()
	if want := "https://" + strings.TrimPrefix(f.url, "http://") + "/kod/"; login.State != provider.Pending ||
		login.Code != "ABC123" || login.URL != want || time.Until(login.Expires) <= 0 {
		t.Fatalf("waiting for the user: %+v, want the code and to enter it at %s", login, want)
	}

	f.set(func() { f.code = "entered" }) // the user enters the code
	<-changes
	if login := p.Login(); login != (provider.Login{State: provider.SignedIn}) {
		t.Errorf("signed in: %+v", login)
	}
	// The free channels with a picture that no other tuner has, each at WP's
	// number for it.
	if got, _ := p.Channels(t.Context()); !slices.Equal(got, channels) {
		t.Errorf("Channels() = %v, want %v", got, channels)
	}

	// The account outlasts a restart, with its session.
	_, restarted := serve(t, db)
	if got, _ := restarted.Channels(t.Context()); restarted.Login().State != provider.SignedIn || !slices.Equal(got, channels) {
		t.Errorf("after a restart: %+v with channels %v, want the account and %v", restarted.Login(), got, channels)
	}
	_, leave := tune(t, restarted, "9")
	leave()

	if err := p.SignOut(); err != nil {
		t.Fatal(err)
	}
	<-changes
	if got, _ := p.Channels(t.Context()); len(got) != 0 || p.Login() != (provider.Login{}) {
		t.Errorf("signed out: channels %v, sign-in %+v", got, p.Login())
	}
	if kept, err := load(db); kept != nil || err != nil {
		t.Errorf("signed out, the store still has %+v, %v", kept, err)
	}
}

func TestSignInFails(t *testing.T) {
	for code, want := range map[string]string{
		"code_not_exists": codeExpired,
		"user_blocked":    "WP refused the sign-in: Unprocessable Entity user_blocked.",
	} {
		f, p := signedIn(t)
		f.set(func() { f.code = code })
		if err := p.SignIn(t.Context()); err != nil {
			t.Fatal(err)
		}
		// The account that was there stays.
		if login := providertest.Await(t, p, provider.SignedIn); login.Problem != want {
			t.Errorf("code answered with %s: %+v, want the problem %q", code, login, want)
		}
	}

	// A code runs out by itself, should WP not say so.
	_, p := serve(t, nil)
	p.codeLife = 20 * time.Millisecond
	if err := p.SignIn(t.Context()); err != nil {
		t.Fatal(err)
	}
	if login := providertest.Await(t, p, provider.SignedOut); login != (provider.Login{Problem: codeExpired}) {
		t.Errorf("code that ran out: %+v", login)
	}
}

func TestSignOutGivesUpTheCode(t *testing.T) {
	_, p := signedIn(t)
	if err := p.SignIn(t.Context()); err != nil {
		t.Fatal(err)
	}
	providertest.Await(t, p, provider.Pending)
	if err := p.SignOut(); err != nil {
		t.Fatal(err)
	}
	if login := p.Login(); login != (provider.Login{State: provider.SignedIn}) {
		t.Errorf("after giving up the code: %+v, want the account kept", login)
	}

	// Signing out while WP is still asked for a code leaves no code behind.
	f, p := serve(t, nil)
	f.held, f.holding = make(chan struct{}), make(chan struct{})
	asked := make(chan error, 1)
	go func() { asked <- p.SignIn(t.Context()) }()
	<-f.holding
	if err := p.SignOut(); err != nil {
		t.Fatal(err)
	}
	close(f.held)
	if err := <-asked; err != nil || p.Login() != (provider.Login{}) {
		t.Errorf("sign-in overtaken by a sign-out = %v, sign-in %+v: want it dropped", err, p.Login())
	}
}

// TestSignOutThatCannotBeSaved checks that an account still in the store is
// not shown as forgotten: it would be back after a restart.
func TestSignOutThatCannotBeSaved(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_, p := serve(t, db)
	p.account = &account{ID: "sid", Val: "val1"}
	db.Close()

	if err := p.SignOut(); err == nil || p.Login().State != provider.SignedIn || p.Login().Problem == "" {
		t.Errorf("SignOut() with the store closed = %v, sign-in %+v: want an error, and the account kept with the problem", err, p.Login())
	}
}

// TestConsents checks that an account WP plays nothing to, for want of its
// consents, is told so for as long as that lasts.
func TestConsents(t *testing.T) {
	f, p := serve(t, nil)
	f.set(func() { f.consents, f.code = true, "entered" })
	if err := p.SignIn(t.Context()); err != nil {
		t.Fatal(err)
	}
	if login := providertest.Await(t, p, provider.SignedIn); login.Problem != consents {
		t.Errorf("signed in without the consents: %+v, want to be told of them", login)
	}
	// The guide is when telesfor learns that they have been accepted.
	listed, _ := p.Channels(t.Context())
	f.set(func() {
		f.consents, f.guide[9] = false, `{"start": "2026-10-09T18:00:00Z", "end": "2026-10-09T19:00:00Z", "title": "Wydarzenia"}`
	})
	guide := func() {
		t.Helper()
		from := time.Date(2026, 10, 9, 18, 0, 0, 0, time.UTC)
		if _, err := p.Programmes(t.Context(), listed, from, from.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	if guide(); p.Login().Problem != "" {
		t.Errorf("after the guide, with the consents accepted: %+v", p.Login())
	}
	f.set(func() { f.consents = true })
	if guide(); p.Login().Problem != consents {
		t.Errorf("after the guide, with the consents withdrawn: %+v", p.Login())
	}

	// So is a channel that opens.
	f.set(func() { f.refusal = "rodo_agreements_required" })
	p.problem = ""
	if _, err := p.Stream(t.Context(), "9"); err == nil || !strings.Contains(err.Error(), "consents") || p.Login().Problem != consents {
		t.Errorf("Stream() without the consents = %v, sign-in %+v: want both to tell of them", err, p.Login())
	}
	f.set(func() { f.refusal = "" })
	_, leave := tune(t, p, "9")
	leave()
	if login := p.Login(); login != (provider.Login{State: provider.SignedIn}) {
		t.Errorf("after a channel opened: %+v, want no word of the consents", login)
	}
	// Another problem is not theirs to clear.
	p.problem = "The sign-in could not be saved."
	_, leave = tune(t, p, "9")
	leave()
	if login := p.Login(); login.Problem != "The sign-in could not be saved." {
		t.Errorf("after a channel opened: %+v, want the problem that was there", login)
	}
}

func TestStream(t *testing.T) {
	f, p := signedIn(t)
	source, leave := tune(t, p, "9")
	if source.URL != f.url+"/cdn/9/playlist.m3u8?t=tok1" || source.Client != p.client {
		t.Errorf("Stream() = %+v, want the first of WP's HLS streams and the provider's client", source)
	}
	// As WP's own player in a browser asks, which is all WP opens a stream to.
	if want := []string{"9 as web from " + f.url + "/tv/ by " + browser}; !slices.Equal(f.opened, want) {
		t.Errorf("opened %q, want %q", f.opened, want)
	}

	// The stream's servers answer to who opened the stream, and are none of
	// the session's business. Segments go over a pool of their own.
	segments, release := httpclient.SegmentClient(source.Client)
	defer release()
	for client, file := range map[*http.Client]string{source.Client: "playlist.m3u8", segments: "media.ts"} {
		resp, _ := providertest.Get(t, client, f.url+"/cdn/9/"+file)
		if want := file + " by " + browser + " with "; resp.StatusCode != http.StatusOK || !slices.Contains(see(f, func() []string { return f.fetched }), want) {
			t.Errorf("%s: %s, asked for as %q: want it asked for as %q", file, resp.Status, f.fetched, want)
		}
	}

	// WP is told that the stream is watched for as long as it is, and that
	// it is over when the viewer leaves.
	for range 2000 {
		if see(f, func() int { return f.beats }) >= 2 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if beats, closed := see(f, func() int { return f.beats }), see(f, func() []string { return f.closed }); beats < 2 || len(closed) != 0 {
		t.Errorf("while watched: %d signs of life and %q closed, want signs of life and nothing closed", beats, closed)
	}
	leave()
	if !slices.Equal(f.closed, []string{"tok1"}) {
		t.Errorf("after the viewer left: closed %q, want the stream's session", f.closed)
	}

	// The viewers of a channel share its session: closing the one of a viewer
	// who came later would stop the channel for the others. Another channel
	// has a session of its own.
	first, leaveFirst := tune(t, p, "9")
	second, leaveSecond := tune(t, p, "9")
	_, leaveOther := tune(t, p, "158")
	if opens := see(f, func() int { return len(f.opened) }); second.URL != first.URL || opens != 3 {
		t.Errorf("two viewers of a channel and one of another: %d streams opened so far, the second viewer at %s: want 3, and the first one's %s", opens, second.URL, first.URL)
	}
	leaveSecond()
	leaveOther()
	if closed := closes(f, 2); !slices.Equal(closed, []string{"tok1", "tok3"}) {
		t.Errorf("with one viewer left on the channel: closed %q, want the other channel's session alone", closed)
	}
	leaveFirst()
	if !slices.Equal(f.closed, []string{"tok1", "tok3", "tok2"}) {
		t.Errorf("after the last viewer left: closed %q, want the channel's session too", f.closed)
	}
	f.set(func() { f.opened, f.closed = f.opened[:1], f.closed[:1] })

	// Viewers who leave before WP has answered leave no session behind: the
	// one it is opened for, and one who waits for it with them.
	f.set(func() { f.waiting, f.opening = make(chan struct{}), make(chan struct{}) })
	ctx, cancel := context.WithCancel(t.Context())
	gone, waited := make(chan error, 1), make(chan error, 1)
	go func() {
		_, err := p.Stream(ctx, "9")
		gone <- err
	}()
	<-f.opening
	waiting, giveUp := context.WithCancel(t.Context())
	go func() {
		_, err := p.Stream(waiting, "9")
		waited <- err
	}()
	for range 2000 {
		if viewers(p) == 2 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	giveUp()
	if err := <-waited; err == nil || viewers(p) != 1 {
		t.Fatalf("Stream() for a viewer who gave up waiting = %v, with %d taken to be watching: want an error, and the first viewer alone", err, viewers(p))
	}
	cancel()
	close(f.waiting)
	if err := <-gone; err != nil {
		t.Errorf("Stream() for a viewer who left = %v", err)
	}
	p.watching.Wait()
	f.set(func() { f.waiting = nil })
	if !slices.Equal(f.closed, []string{"tok1", "tok2"}) || viewers(p) != 0 {
		t.Errorf("after the viewers left early: closed %q with %d taken to be watching, want that session too, and nobody", f.closed, viewers(p))
	}

	// A viewer who changes channel with the account full is not turned away
	// for the stream they have just left, which WP has yet to hear is over.
	shutting, full := make(chan struct{}), make(chan struct{})
	f.set(func() { f.slots, f.shutting, f.full = 1, shutting, full })
	watching, change := context.WithCancel(t.Context())
	if _, err := p.Stream(watching, "9"); err != nil {
		t.Fatal(err)
	}
	change()
	for range 2000 {
		if viewers(p) == 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	go func() {
		<-full // WP has refused the new channel
		close(shutting)
	}()
	_, leave = tune(t, p, "158")
	leave()
	f.set(func() { f.slots, f.shutting = 0, nil })
	if opens := f.opened[len(f.opened)-3:]; !strings.HasPrefix(opens[0], "9 ") || !strings.HasPrefix(opens[1], "158 ") || !strings.HasPrefix(opens[2], "158 ") {
		t.Errorf("a change of channel with the account full: opened %q, want the new channel asked for again once the old was closed", opens)
	}
}

func TestStreamRefused(t *testing.T) {
	tests := []struct {
		refusal string // WP's name for it
		want    string // what the error must mention
	}{
		{"user_outside_eu", "-wppilot-proxy"},
		{"user_not_verified_eu", "-wppilot-proxy"},
		{"user_channel_proxy_detected", "refused at this address"},
		{"multiroom_limit_exceeded", "with Polsat HD, TVN playing"},
		{"stream_consumption_over_limit", "WP Pilot answers 422 stream_consumption_over_limit"},
		{"channel_switch_limit", "WP Pilot answers 422 channel_switch_limit"},
	}
	for _, test := range tests {
		t.Run(test.refusal, func(t *testing.T) {
			f, p := signedIn(t)
			f.set(func() { f.refusal = test.refusal })

			_, err := p.Stream(t.Context(), "9")
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Errorf("Stream() error = %v, want one mentioning %q", err, test.want)
			}
			if got, _ := p.Channels(t.Context()); !slices.Equal(got, channels) || p.Login() != (provider.Login{State: provider.SignedIn}) {
				t.Errorf("after the refusal: channels %v, sign-in %+v: want neither touched", got, p.Login())
			}
		})
	}

	_, p := signedIn(t)
	if _, err := p.Stream(t.Context(), "16"); err == nil || !strings.Contains(err.Error(), "no channel") {
		t.Errorf("Stream() of a channel the account lacks = %v", err)
	}
	_, signedOut := serve(t, nil)
	if _, err := signedOut.Stream(t.Context(), "9"); err == nil || !strings.Contains(err.Error(), "sign in") {
		t.Errorf("Stream() without an account = %v, want a call to sign in", err)
	}
}

// TestStreamExpiresTheSignIn checks that a stream WP refuses to a session it
// no longer knows expires the sign-in.
func TestStreamExpiresTheSignIn(t *testing.T) {
	f, p := signedIn(t)
	f.set(func() { f.revoked = true })
	if _, err := p.Stream(t.Context(), "9"); err == nil || !strings.Contains(err.Error(), "sign in again") || p.Login().State != provider.Expired {
		t.Errorf("Stream() with a session WP dropped = %v, sign-in %+v: want it expired", err, p.Login())
	}
}

// TestUnplayable checks that a channel telesfor cannot play leaves the
// lineup once that is found out, and that a channel which only fails does not.
func TestUnplayable(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	f, p := serve(t, db)
	p.account = &account{ID: "sid", Val: "val1", Channels: slices.Clone(lineup)}
	if err := save(db, p.account); err != nil {
		t.Fatal(err)
	}
	var changes atomic.Int32
	p.OnChange(func() { changes.Add(1) })
	listed := func(p *Provider) string {
		var ids []string
		got, _ := p.Channels(t.Context())
		for _, ch := range got {
			ids = append(ids, ch.ID)
		}
		return strings.Join(ids, " ")
	}

	// WP names licence servers for channels it streams in the clear too.
	f.set(func() { f.licensed = map[int]bool{12: true, 158: true}; f.encrypted = map[int]bool{12: true} })
	_, leave := tune(t, p, "158")
	leave()
	// A playlist that cannot be read tells nothing about the channel.
	f.set(func() { f.shut = map[int]bool{12: true} })
	if _, err := p.Stream(t.Context(), "12"); err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Errorf("Stream() of a channel whose playlist is refused = %v", err)
	}
	if listed(p) != "9 12 158" || changes.Load() != 0 {
		t.Fatalf("channels %s after %d changes, want all three still", listed(p), changes.Load())
	}

	f.set(func() { f.shut = nil })
	if _, err := p.Stream(t.Context(), "12"); err == nil || !strings.Contains(err.Error(), "DRM") {
		t.Errorf("Stream() of an encrypted channel = %v, want a DRM error", err)
	}
	f.set(func() { f.dashOnly = map[int]bool{9: true} })
	if _, err := p.Stream(t.Context(), "9"); err == nil || !strings.Contains(err.Error(), "no HLS stream") {
		t.Errorf("Stream() of a channel in DASH alone = %v", err)
	}
	if listed(p) != "158" || changes.Load() != 2 {
		t.Errorf("channels %s after %d changes, want Telewizja WP alone after two", listed(p), changes.Load())
	}
	// None of the four streams is left open at WP.
	if want := []string{"tok1", "tok2", "tok3", "tok4"}; !slices.Equal(f.closed, want) {
		t.Errorf("closed %q, want %q", f.closed, want)
	}
	if _, restarted := serve(t, db); listed(restarted) != "158" {
		t.Errorf("channels after a restart: %s, want Telewizja WP alone", listed(restarted))
	}

	// What was found out holds for as long as the account stays: also when
	// it signs in anew, as it does after its session has expired.
	f.set(func() { f.code = "entered" })
	if err := p.SignIn(t.Context()); err != nil {
		t.Fatal(err)
	}
	for range 2000 {
		if p.Login().State == provider.SignedIn && changes.Load() == 3 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if listed(p) != "158" {
		t.Errorf("channels after signing in anew: %s, want Telewizja WP alone", listed(p))
	}
}

// TestSession checks that the session's cookies are kept as WP reissues
// them, and go to WP's API alone.
func TestSession(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	f, p := serve(t, db)
	old := &account{ID: "sid", Val: "val1", Channels: slices.Clone(lineup)}
	p.account = old
	if err := save(db, p.account); err != nil {
		t.Fatal(err)
	}

	f.set(func() { f.reissue = "val2" })
	_, leave := tune(t, p, "9") // opened with the old one, closed with the new
	leave()
	if kept, _ := load(db); p.account.Val != "val2" || kept == nil || kept.Val != "val2" || !slices.Equal(f.closed, []string{"tok1"}) {
		t.Errorf("after WP reissued the session: %+v, in the store %+v, closed %q", p.account, kept, f.closed)
	}

	// Calls that cross: WP reissues the session in its answer to one, and
	// refuses the other, which came with the session as it was. That is no
	// sign-in expired, and the call is made again.
	f.set(func() {
		f.crossed = func() {
			f.set(func() { f.val = "val3" })
			p.renew(old, "", "val3")
		}
	})
	_, leave = tune(t, p, "9")
	leave()
	if login := p.Login(); login != (provider.Login{State: provider.SignedIn}) || old.Val != "val3" {
		t.Errorf("after calls crossed: sign-in %+v with the session %s, want it signed in with the one reissued", login, old.Val)
	}

	// A session is its account's. A viewer of an account that has signed in
	// anew shares none with one who came before, and each session is closed
	// as the account that opened it, signed out since or not.
	f.set(func() { f.opened, f.closed = nil, nil })
	before, leaveBefore := context.WithCancel(t.Context())
	after, leaveAfter := context.WithCancel(t.Context())
	earlier, err := p.Stream(before, "9")
	if err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	p.account = &account{ID: "sid", Val: "val3", Channels: slices.Clone(lineup)}
	p.mu.Unlock()
	later, err := p.Stream(after, "9")
	if err != nil {
		t.Fatal(err)
	}
	if later.URL == earlier.URL {
		t.Errorf("a viewer of the account signed in anew watches %s, as the one before: want a session of the account's own", later.URL)
	}
	p.mu.Lock()
	p.account = old
	p.mu.Unlock()
	leaveBefore()
	leaveAfter()
	p.watching.Wait()
	if closed := closes(f, 2); len(closed) != 2 {
		t.Errorf("after both left: closed %q, want the two sessions", closed)
	}

	// A stream's signs of life go where WP says, but the session does not.
	var strayed []string
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.set(func() { strayed = append(strayed, r.Header.Get("Cookie")) })
		w.WriteHeader(http.StatusNoContent)
	}))
	defer elsewhere.Close()
	f.set(func() { f.pulse = elsewhere.URL + "/heartbeat" })
	_, leave = tune(t, p, "9")
	for range 2000 {
		if see(f, func() int { return len(strayed) }) > 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	leave()
	if len(strayed) == 0 || strayed[0] != "" {
		t.Errorf("signs of life sent elsewhere came with the cookies %q, want them sent and without any", strayed)
	}

	// To a session it no longer knows, WP answers with a guest's: that is
	// not the account's to take.
	f.set(func() { f.revoked = true })
	p.verify(t.Context())
	if p.relist(t.Context()); p.account.ID != "sid" || p.account.Val != "val3" || p.Login().State != provider.Expired {
		t.Errorf("after WP dropped the session: %+v, sign-in %+v: want it kept, and expired", p.account, p.Login())
	}

	// An account that has signed out is not put back by an answer to it.
	if err := p.SignOut(); err != nil {
		t.Fatal(err)
	}
	p.renew(old, "", "val3")
	if kept, err := load(db); kept != nil || err != nil {
		t.Errorf("after an answer to an account signed out, the store has %+v, %v", kept, err)
	}
}

func TestProgrammes(t *testing.T) {
	f, p := signedIn(t)
	from := time.Date(2026, 10, 9, 18, 0, 0, 0, time.UTC)
	f.set(func() {
		f.guide[9] = `
			{"start": "2026-10-09T16:00:00Z", "end": "2026-10-09T17:30:00Z", "title": "Over by then"},
			{"start": "2026-10-09T17:30:00Z", "end": "2026-10-09T18:30:00Z", "title": "Wydarzenia", "description": "Wiadomości", "photo": "https://img.example/w.jpg"},
			{"start": "2026-10-09T18:30:00Z", "end": "2026-10-09T19:15:00Z", "title": ""},
			{"start": "2026-10-09T19:15:00Z", "end": "2026-10-09T20:00:00Z", "title": "Film", "description": null, "photo": null},
			{"start": "2026-10-09T22:00:00Z", "end": "2026-10-09T23:00:00Z", "title": "Too late"}`
		f.guide[158] = `{"start": "2026-10-09T18:00:00Z", "end": "2026-10-09T22:30:00Z", "title": "Maraton"}`
	})
	got, err := p.Programmes(t.Context(), channels, from, from.Add(4*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	var shown []string
	for _, programme := range got {
		shown = append(shown, fmt.Sprintf("%s %s-%s %s | %s | %s", programme.ChannelID,
			programme.Start.UTC().Format("15:04"), programme.Stop.UTC().Format("15:04"), programme.Title, programme.Description, programme.Image))
	}
	want := []string{
		// On air at the start of the guide.
		"9 17:30-18:30 Wydarzenia | Wiadomości | https://img.example/w.jpg",
		// What WP lists without a title, and what it does not list, has
		// the channel's name by the clock's hours.
		"9 18:30-19:00 Polsat HD | " + unlisted + " | ",
		"9 19:00-19:15 Polsat HD | " + unlisted + " | ",
		"9 19:15-20:00 Film |  | ",
		"9 20:00-21:00 Polsat HD | " + unlisted + " | ",
		"9 21:00-22:00 Polsat HD | " + unlisted + " | ",
		// WP has no guide of every channel.
		"12 18:00-19:00 TV Puls HD | " + unlisted + " | ",
		"12 19:00-20:00 TV Puls HD | " + unlisted + " | ",
		"12 20:00-21:00 TV Puls HD | " + unlisted + " | ",
		"12 21:00-22:00 TV Puls HD | " + unlisted + " | ",
		"158 18:00-22:30 Maraton |  | ",
	}
	if !slices.Equal(shown, want) {
		t.Errorf("Programmes() =\n%s\nwant\n%s", strings.Join(shown, "\n"), strings.Join(want, "\n"))
	}
	// The guide is anyone's to read: the session stays out of it.
	if want := []string{"9,12,158 with "}; !slices.Equal(f.guides, want) {
		t.Errorf("guide requests %q, want %q", f.guides, want)
	}

	// A guide of more channels is asked for in WP's own portions.
	var many []provider.Channel
	for id := range 21 {
		many = append(many, provider.Channel{ID: strconv.Itoa(id + 1), Name: "Channel"})
	}
	f.set(func() { f.guides, f.guide[1] = nil, f.guide[158] })
	if _, err := p.Programmes(t.Context(), many, from, from.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if len(f.guides) != 2 || strings.Count(f.guides[0], ",") != 19 || f.guides[1] != "21 with " {
		t.Errorf("guide requests for 21 channels: %q, want 20 and 1", f.guides)
	}

	// A guide with nothing on any channel is none.
	f.set(func() { clear(f.guide) })
	if got, err := p.Programmes(t.Context(), channels, from, from.Add(time.Hour)); err == nil {
		t.Errorf("guide without a programme = %v, want an error", got)
	}
}

// TestProgrammesLookAfterTheAccount checks what else the guide is the time
// for: the channels WP gives the account, and whether it still accepts it.
func TestProgrammesLookAfterTheAccount(t *testing.T) {
	f, p := signedIn(t)
	f.set(func() {
		f.guide[9] = `{"start": "2026-10-09T18:00:00Z", "end": "2026-10-09T19:00:00Z", "title": "Wydarzenia"}`
	})
	from := time.Date(2026, 10, 9, 18, 0, 0, 0, time.UTC)
	// Signed in when WP gave the account less, and under another name.
	p.account.Channels = []channel{{ID: 9, Name: "Polsat"}}
	changed := false
	p.OnChange(func() { changed = true })
	if _, err := p.Programmes(t.Context(), channels[:1], from, from.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got, _ := p.Channels(t.Context()); !changed || !slices.Equal(got, channels) {
		t.Errorf("after the guide: changed %t, channels %v, want %v", changed, got, channels)
	}
	if login := p.Login(); login.State != provider.SignedIn {
		t.Errorf("sign-in after the guide: %+v", login)
	}

	f.set(func() { f.revoked = true })
	if _, err := p.Programmes(t.Context(), channels, from, from.Add(time.Hour)); err != nil || p.Login().State != provider.Expired {
		t.Errorf("guide with a session WP dropped: %v, sign-in %+v, want it expired", err, p.Login())
	}
	// A list that cannot be had takes no channel away.
	if got, _ := p.Channels(t.Context()); !slices.Equal(got, channels) {
		t.Errorf("channels after WP dropped the session: %v, want %v", got, channels)
	}
}
