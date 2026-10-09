package globo

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/combor/telesfor/internal/provider"
	"github.com/combor/telesfor/internal/provider/providertest"
	"github.com/combor/telesfor/internal/store"
)

// catalogue is Globoplay's list of channels: in its own order, and with a
// channel that needs a subscription.
const catalogue = `[
	{"slug": "ge-tv", "name": "ge tv", "mediaId": "33", "logo": "https://img.example/ge.png"},
	{"slug": "sportv", "name": "sportv", "mediaId": "44"},
	{"slug": "tv-globo", "name": "TV Globo", "mediaId": "11", "logo": "https://img.example/globo.png"},
	{"slug": "futura", "name": "Futura", "mediaId": "22"}
]`

// channels is what the provider makes of the catalogue.
var channels = []provider.Channel{
	{ID: "tv-globo", Name: "TV Globo", Logo: "https://img.example/globo.png", Place: 1},
	{ID: "futura", Name: "Futura", Place: 2},
	{ID: "ge-tv", Name: "ge tv", Logo: "https://img.example/ge.png", Place: 3},
}

// globo is a fake of Globo's APIs, as much of them as the provider uses.
type globo struct {
	code    atomic.Int32  // how Globo answers a question about the code; 401 until the user enters it
	revoked atomic.Bool   // Globo no longer accepts the session
	guide   string        // the data of a guide answer
	errors  string        // the errors beside it, if any
	held    chan struct{} // if set, a request for a code waits for it to close
	holding chan struct{} // closed when the first such request waits
	refusal string        // the playback API's, with its status in front, as in "403 geo-block"
	drm     bool
	asked   atomic.Pointer[string] // the last GraphQL or playback request
}

// serve starts a fake Globo and returns it with a provider that talks to it
// and keeps its sign-in in db.
func serve(t *testing.T, db *bolt.DB) (*globo, *Provider) {
	t.Helper()
	g := &globo{}
	g.code.Store(http.StatusUnauthorized)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/device/request-login/for/4654", func(w http.ResponseWriter, r *http.Request) {
		if g.held != nil {
			close(g.holding)
			<-g.held
		}
		io.WriteString(w, `{"user_code": "ABCDEFGH", "token": "device-token"}`)
	})
	mux.HandleFunc("POST /api/device/auth-with-token/for/4654", func(w http.ResponseWriter, r *http.Request) {
		if body, _ := io.ReadAll(r.Body); !strings.Contains(string(body), `"device-token"`) {
			t.Errorf("asked about the code with %s", body)
		}
		w.WriteHeader(int(g.code.Load()))
		if g.code.Load() == http.StatusOK {
			io.WriteString(w, `{"status": "AUTHENTICATED", "glbid": "session"}`)
		}
	})
	mux.HandleFunc("GET /api/user", func(w http.ResponseWriter, r *http.Request) {
		if cookie, err := r.Cookie("GLBID"); err != nil || cookie.Value != "session" || g.revoked.Load() {
			w.WriteHeader(http.StatusNotFound)
		}
	})
	mux.HandleFunc("GET /affiliate", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"code": "SP1", "name": "GLOBO SAO PAULO"}`)
	})
	mux.HandleFunc("POST /graphql", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-tenant-id") != "globo-play" || r.Header.Get("x-client-version") == "" {
			t.Errorf("GraphQL request without its headers: %v", r.Header)
		}
		body, _ := io.ReadAll(r.Body)
		asked := string(body)
		g.asked.Store(&asked)
		if strings.Contains(asked, "epgByDate") {
			io.WriteString(w, `{"data": `+g.guide+g.errors+`}`)
			return
		}
		io.WriteString(w, `{"data": {"broadcasts": `+catalogue+`}}`)
	})
	mux.HandleFunc("POST /playback", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		asked := r.Header.Get("Authorization") + " " + string(body)
		g.asked.Store(&asked)
		if status, code, refused := strings.Cut(g.refusal, " "); refused {
			var n int
			fmt.Sscan(status, &n)
			w.WriteHeader(n)
			fmt.Fprintf(w, `{"code": %q, "reason": ""}`, code)
			return
		}
		fmt.Fprintf(w, `{"sources": [{"url": "http://%s/live/token/playlist.m3u8"}], "resource": {"drm_protection_enabled": %t}}`, r.Host, g.drm)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	p := &Provider{
		client:    server.Client(),
		db:        db,
		login:     server.URL,
		graphql:   server.URL + "/graphql",
		playback:  server.URL + "/playback",
		affiliate: server.URL + "/affiliate",
		poll:      time.Millisecond,
		codeLife:  time.Minute,
	}
	var err error
	if p.account, err = load(db); err != nil {
		t.Fatal(err)
	}
	return g, p
}

// signedIn returns a fake Globo and a provider that has an account with it.
func signedIn(t *testing.T) (*globo, *Provider) {
	t.Helper()
	g, p := serve(t, nil)
	p.account = &account{GLBID: "session", Channels: list(json.RawMessage(catalogue))}
	return g, p
}

func TestSignIn(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	g, p := serve(t, db)
	changes := make(chan struct{}, 2)
	p.OnChange(func() { changes <- struct{}{} })

	if got, _ := p.Channels(t.Context()); len(got) != 0 || p.Login() != (provider.Login{}) {
		t.Fatalf("before signing in: channels %v, sign-in %+v", got, p.Login())
	}
	if err := p.SignIn(t.Context()); err != nil {
		t.Fatal(err)
	}
	login := p.Login()
	if login.State != provider.Pending || login.Code != "ABCDEFGH" || login.URL != activationURL || time.Until(login.Expires) <= 0 {
		t.Fatalf("waiting for the user: %+v, want the code and where to enter it", login)
	}

	g.code.Store(http.StatusOK) // the user enters the code
	<-changes
	if login := p.Login(); login != (provider.Login{State: provider.SignedIn}) {
		t.Errorf("signed in: %+v", login)
	}
	if got, _ := p.Channels(t.Context()); !slices.Equal(got, channels) {
		t.Errorf("Channels() = %v, want %v", got, channels)
	}

	// The account outlasts a restart.
	if _, restarted := serve(t, db); restarted.Login().State != provider.SignedIn {
		t.Errorf("after a restart: %+v, want the account", restarted.Login())
	} else if got, _ := restarted.Channels(t.Context()); !slices.Equal(got, channels) {
		t.Errorf("Channels() after a restart = %v, want %v", got, channels)
	}

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
	for status, want := range map[int]string{
		http.StatusNotFound:            codeExpired,
		http.StatusInternalServerError: "Globo refused the sign-in: Internal Server Error.",
	} {
		g, p := signedIn(t)
		g.code.Store(int32(status))
		if err := p.SignIn(t.Context()); err != nil {
			t.Fatal(err)
		}
		// The account that was there stays.
		if login := providertest.Await(t, p, provider.SignedIn); login.Problem != want {
			t.Errorf("code answered with %d: %+v, want the problem %q", status, login, want)
		}
	}

	// A code runs out by itself, should Globo not say so.
	_, p := serve(t, nil)
	p.codeLife = 20 * time.Millisecond
	if err := p.SignIn(t.Context()); err != nil {
		t.Fatal(err)
	}
	p.Login() // Pending, for a moment
	for range 2000 {
		if p.Login().Problem != "" {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if login := p.Login(); login != (provider.Login{Problem: codeExpired}) {
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

	// Signing out while Globo is still asked for a code leaves no code behind.
	g, p := serve(t, nil)
	g.held, g.holding = make(chan struct{}), make(chan struct{})
	asked := make(chan error, 1)
	go func() { asked <- p.SignIn(t.Context()) }()
	<-g.holding
	if err := p.SignOut(); err != nil {
		t.Fatal(err)
	}
	close(g.held)
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
	p.account = &account{GLBID: "session"}
	db.Close()

	if err := p.SignOut(); err == nil || p.Login().State != provider.SignedIn || p.Login().Problem == "" {
		t.Errorf("SignOut() with the store closed = %v, sign-in %+v: want an error, and the account kept with the problem", err, p.Login())
	}
}

func TestProgrammes(t *testing.T) {
	g, p := signedIn(t)
	from := time.Date(2026, 10, 4, 22, 0, 0, 0, brt)
	at := func(hour, minute int) int64 { return time.Date(2026, 10, 4, hour, minute, 0, 0, brt).Unix() }
	film := fmt.Sprintf(`{"name": "Filme", "description": "Um filme", "startTime": %d, "endTime": %d,
		"title": {"poster": {"web": "https://img.example/filme.jpg"}}}`, at(23, 30), at(25, 0)-1)
	g.guide = fmt.Sprintf(`{
		"all": %s,
		"c0": {
			"d0": {"entries": [
				{"name": "Over by then", "startTime": %d, "endTime": %d},
				{"name": "Jornal", "startTime": %d, "endTime": %d, "title": null},
				%s
			]},
			"d1": {"entries": [%s, {"name": "Too late", "startTime": %d, "endTime": %d}]}
		},
		"c1": {"d0": {"entries": [{"name": "Aula", "startTime": %d, "endTime": %d}]}, "d1": {"entries": []}},
		"c2": {"d0": {"entries": [{"name": "Jogo", "startTime": %d, "endTime": %d}]}, "d1": {"entries": []}}
	}`, catalogue,
		at(20, 0), at(22, 0)-1, at(21, 0), at(23, 30)-1, film, film, at(47, 0), at(48, 0),
		at(22, 0), at(23, 0), at(22, 0), at(23, 0))

	got, err := p.Programmes(t.Context(), channels, from, from.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	clock := func(hour, minute int) time.Time { return time.Unix(at(hour, minute), 0).In(brt) }
	want := []provider.Programme{
		// On air at the start of the guide, and ending as the next begins.
		{ChannelID: "tv-globo", Title: "Jornal", Start: clock(21, 0), Stop: clock(23, 30)},
		// On air at midnight, so listed in both days.
		{ChannelID: "tv-globo", Title: "Filme", Description: "Um filme", Image: "https://img.example/filme.jpg", Start: clock(23, 30), Stop: clock(25, 0)},
		{ChannelID: "futura", Title: "Aula", Start: clock(22, 0), Stop: clock(23, 0)},
		{ChannelID: "ge-tv", Title: "Jogo", Start: clock(22, 0), Stop: clock(23, 0)},
	}
	if !slices.Equal(got, want) {
		t.Errorf("Programmes() = %v, want %v", got, want)
	}
	// Each channel by its video, for both days the guide spans, for the region
	// Globo serves.
	if missing := lacks(*g.asked.Load(), `mediaId: \"11\"`, `mediaId: \"33\"`, `d0: epgByDate(date: \"2026-10-04\")`,
		`d1: epgByDate(date: \"2026-10-05\")`, `"region":{"affiliateCode":"SP1"}`); missing != nil || strings.Contains(*g.asked.Load(), "d2:") {
		t.Errorf("guide request lacks %q, or asks for a third day: %s", missing, *g.asked.Load())
	}

	// The guide is when Globo is asked whether it still accepts the account.
	if login := p.Login(); login.State != provider.SignedIn {
		t.Errorf("sign-in after the guide: %+v", login)
	}
	g.revoked.Store(true)
	if _, err := p.Programmes(t.Context(), channels, from, from.Add(24*time.Hour)); err != nil || p.Login().State != provider.Expired {
		t.Errorf("guide with a session Globo dropped: %v, sign-in %+v, want it expired", err, p.Login())
	}

	// Half a guide is none.
	g.guide = `{"all": [], "c0": null, "c1": null, "c2": null}`
	if got, err := p.Programmes(t.Context(), channels, from, from.Add(24*time.Hour)); err == nil {
		t.Errorf("guide without the channels = %v, want an error", got)
	}
}

// TestProgrammesFollowTheCatalogue checks that a channel whose video changed
// is found again, by the list that comes with every guide, and that no
// channel changes its place in the lineup, which is what Plex knows it by.
func TestProgrammesFollowTheCatalogue(t *testing.T) {
	g, p := signedIn(t)
	all := slices.Clone(p.account.Channels)
	// Signed in while Globoplay had no Futura, and before TV Globo moved.
	p.account.Channels = []channel{{ID: "tv-globo", Name: "TV Globo", Media: "old"}, all[2]}
	if got, _ := p.Channels(t.Context()); len(got) != 2 || got[1] != channels[2] {
		t.Fatalf("Channels() without Futura = %+v, want ge tv in its own place all the same", got)
	}
	changed := false
	p.OnChange(func() { changed = true })
	// Globo's answer about a video it no longer has: an error beside the list.
	g.errors = `, "errors": [{"message": "network timeout at: https://backstage.example/findOne"}]`
	guide := func(catalogue string) {
		g.guide = `{"all": ` + catalogue + `, "c0": null}`
		if _, err := p.Programmes(t.Context(), channels[:1], time.Now(), time.Now().Add(time.Hour)); err == nil {
			t.Error("guide answered with an error was accepted")
		}
	}

	guide(catalogue)
	if !changed || !slices.Equal(p.account.Channels, all) {
		t.Errorf("after the guide: changed %t, channels %+v, want %+v", changed, p.account.Channels, all)
	}
	// A list that lacks a channel, as Globo's sometimes does, takes none away.
	guide(`[{"slug": "futura", "name": "Futura", "mediaId": "22"}]`)
	if !slices.Equal(p.account.Channels, all) {
		t.Errorf("after a guide with a short list: channels %+v, want %+v", p.account.Channels, all)
	}
}

func TestStream(t *testing.T) {
	g, p := signedIn(t)
	source, err := p.Stream(t.Context(), "futura")
	if err != nil {
		t.Fatal(err)
	}
	if source.URL != p.login+"/live/token/playlist.m3u8" || source.Client != p.client {
		t.Errorf("Stream() = %+v, want Globo's stream and the provider's client", source)
	}
	if missing := lacks(*g.asked.Load(), "Bearer session ", `"video_id":"22"`, `"version":2`); missing != nil ||
		strings.Contains(*g.asked.Load(), "dvr") {
		t.Errorf("playback request lacks %q, or asks for the long playlist: %s", missing, *g.asked.Load())
	}

	for refusal, want := range map[string]string{
		"403 geo-block":           "blocked outside Brazil",
		"404 geo-fencing":         "blocked outside Brazil",
		"403 user-not-authorized": "no access to this channel",
		"401 login-required":      "sign in again",
		"404 video-not-found":     "Not Found video-not-found",
	} {
		g.refusal = refusal
		if _, err := p.Stream(t.Context(), "futura"); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Stream() refused with %q = %v, want an error with %q", refusal, err, want)
		}
	}
	if login := p.Login(); login.State != provider.Expired {
		t.Errorf("sign-in after Globo asked for a login: %+v, want it expired", login)
	}
	// A refusal that comes in late says nothing about the account signed in since.
	old := p.account
	p.account, p.expired = &account{GLBID: "session", Channels: old.Channels}, false
	if p.expire(old); p.Login().State != provider.SignedIn {
		t.Errorf("sign-in after a refusal of the account before it: %+v", p.Login())
	}

	g.refusal, g.drm = "", true
	if _, err := p.Stream(t.Context(), "futura"); err == nil || !strings.Contains(err.Error(), "DRM") {
		t.Errorf("Stream() of an encrypted channel = %v, want a DRM error", err)
	}

	_, signedOut := serve(t, nil)
	if _, err := signedOut.Stream(t.Context(), "futura"); err == nil || !strings.Contains(err.Error(), "sign in") {
		t.Errorf("Stream() without an account = %v, want a call to sign in", err)
	}
}

// lacks returns the wants that s does not contain.
func lacks(s string, wants ...string) []string {
	var missing []string
	for _, want := range wants {
		if !strings.Contains(s, want) {
			missing = append(missing, want)
		}
	}
	return missing
}
