package tf1

import (
	"context"
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

	bolt "go.etcd.io/bbolt"

	"github.com/combor/telesfor/internal/httpclient"
	"github.com/combor/telesfor/internal/provider"
	"github.com/combor/telesfor/internal/provider/providertest"
	"github.com/combor/telesfor/internal/store"
)

// lci is what plays without an account, and everything what plays with one.
var (
	lci        = []provider.Channel{{ID: "lci", Name: "LCI", Logo: channels[3].logo, Place: 4}}
	everything = []provider.Channel{
		{ID: "tf1", Name: "TF1", Logo: channels[0].logo, Place: 1},
		{ID: "tfx", Name: "TFX", Logo: channels[1].logo, Place: 2},
		{ID: "tf1-series-films", Name: "TF1 Séries Films", Logo: channels[2].logo, Place: 3},
		lci[0],
	}
)

// tf1 is a fake of TF1: the site that signs devices in, the player's API, the
// servers that hand the streams out, and the guide.
type tf1 struct {
	url string

	mu       sync.Mutex
	code     string          // how TF1 answers a question about the code, as an error of RFC 8628; empty once the user has entered it
	codeLife float64         // of a code, in seconds
	polled   []time.Time     // when it was asked about the code
	held     chan struct{}   // if set, a request for a code waits for it to close
	holding  chan struct{}   // closed when the first such request waits
	tokens   int             // how many pairs of tokens it has handed out: the last is the one it takes
	renewals int             // how many of them for a refresh token
	revoked  bool            // it takes no refresh token any more
	down     bool            // it answers requests for tokens with a 503
	slow     time.Duration   // how long it takes over tokens
	dropped  map[string]bool // the access tokens it takes no more, before their time
	refusal  string          // the player's API's, as its error_code
	drm      bool
	format   string
	asked    []string          // the videos the player's API was asked for, each with the token it was asked with
	passes   int               // how many passes have been handed out
	expired  map[string]bool   // the passes that are good no more
	abroad   bool              // the servers take the viewer to be outside France
	pages    map[string]string // the guide, by channel and day, as in "TF1 2026-10-07"
	days     []string          // the pages of it that were asked for
	closed   bool              // the guide answers with a 503
}

// serve starts a fake TF1 and returns it with a provider that talks to it and
// keeps its sign-in in db.
func serve(t *testing.T, db *bolt.DB) (*tf1, *Provider) {
	t.Helper()
	f := &tf1{code: "authorization_pending", codeLife: 60, format: "hls", dropped: map[string]bool{}, expired: map[string]bool{}}
	grant := func(w http.ResponseWriter) {
		f.tokens++
		fmt.Fprintf(w, `{"access_token": "access%d", "refresh_token": "refresh%d", "token_type": "bearer", "expires_in": 43200}`, f.tokens, f.tokens)
	}
	refuse := func(w http.ResponseWriter, status int, reason string) {
		w.WriteHeader(status)
		fmt.Fprintf(w, `{"error": %q}`, reason)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /token/device/code", func(w http.ResponseWriter, r *http.Request) {
		if f.held != nil {
			close(f.holding)
			<-f.held
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		fmt.Fprintf(w, `{"device_code": "device", "user_code": "1234-5678", "verification_uri": "%s/tv",
			"verification_uri_complete": "%s/tv?user_code=1234-5678", "expires_in": %g, "interval": 0.001}`, f.url, f.url, f.codeLife)
	})
	mux.HandleFunc("POST /token/oauth2", func(w http.ResponseWriter, r *http.Request) {
		var asked struct {
			Grant   string `json:"grant_type"`
			Device  string `json:"device_code"`
			Refresh string `json:"refresh_token"`
		}
		json.NewDecoder(r.Body).Decode(&asked)
		f.mu.Lock()
		slow := f.slow
		f.mu.Unlock()
		time.Sleep(slow)
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case f.down:
			w.WriteHeader(http.StatusServiceUnavailable)
		case asked.Grant == deviceGrant && asked.Device == "device":
			f.polled = append(f.polled, time.Now())
			switch f.code {
			case "":
				grant(w)
			case "slow_down": // once
				f.code = ""
				refuse(w, http.StatusBadRequest, "slow_down")
			case "authorization_pending":
				refuse(w, http.StatusForbidden, f.code)
			default:
				refuse(w, http.StatusBadRequest, f.code)
			}
		case asked.Grant == "refresh_token" && asked.Refresh == fmt.Sprintf("refresh%d", f.tokens) && !f.revoked:
			f.renewals++
			grant(w)
		case asked.Grant == "refresh_token":
			refuse(w, http.StatusUnauthorized, "authentication failed: unexpected refresh token")
		default:
			t.Errorf("asked for tokens with %+v", asked)
			refuse(w, http.StatusUnauthorized, "expired_token")
		}
	})
	mux.HandleFunc("GET /mediainfocombo/{video}", func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.UserAgent(), "iPhone") || r.URL.RawQuery != "context=MYTF1&pver=5015000" {
			t.Errorf("the player's API asked as %q with %q: want an iPhone, and the player's name and version", r.UserAgent(), r.URL.RawQuery)
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		video := r.PathValue("video")
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		f.asked = append(f.asked, strings.TrimSpace(video+" "+token))
		refusal := f.refusal
		switch {
		case refusal != "":
		case token == "" && video != "L_LCI":
			refusal = "PERMISSION_DENIED"
		case token != "" && (token != fmt.Sprintf("access%d", f.tokens) || f.dropped[token]):
			refusal = "AUTH_ERROR"
		}
		if refusal != "" {
			fmt.Fprintf(w, `{"media": {"error_code": %q}, "delivery": {"code": 403, "format": "hls"}}`, refusal)
			return
		}
		f.passes++
		name := strings.TrimPrefix(video, "L_")
		delivery := map[string]any{
			"code": 200, "format": f.format,
			"url": fmt.Sprintf("%s/pass%d/prod/%s/cmaf/out/%s.m3u8", f.url, f.passes, name, name),
		}
		// TF1 itself, with adverts put in: the stream as it is broadcast is
		// what the player falls back on.
		if video == "L_TF1" {
			delivery["fallback"] = map[string]any{"code": 200, "format": f.format, "url": delivery["url"]}
			delivery["url"] = fmt.Sprintf("%s/viewer/pass%d/prod/TF1/cmaf-ssai/out/TF1.m3u8", f.url, f.passes)
		}
		if f.drm {
			delivery["drms"] = []map[string]string{{"name": "fairplay"}, {"name": "widevine"}}
		}
		json.NewEncoder(w).Encode(map[string]any{"media": map[string]any{"id": video}, "delivery": delivery})
	})
	// A stream's files, which the fake has the names of in place of.
	mux.HandleFunc("GET /{pass}/prod/{channel}/cmaf/out/{file}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case f.abroad:
			w.WriteHeader(http.StatusForbidden)
			io.WriteString(w, "edge-vhost/geoip-restriction")
		case f.expired[r.PathValue("pass")]:
			w.WriteHeader(http.StatusForbidden)
			io.WriteString(w, "edge-vhost/invalid-token")
		default:
			io.WriteString(w, r.PathValue("channel")+"/"+r.PathValue("file"))
		}
	})
	mux.HandleFunc("GET /grilles-tv/{channel}/jour/{day}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		page := r.PathValue("channel") + " " + r.PathValue("day")
		f.days = append(f.days, page)
		if f.closed {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		io.WriteString(w, "<html>"+f.pages[page]+"</html>")
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	f.url = server.URL

	p := &Provider{
		client: server.Client(),
		db:     db,
		site:   server.URL,
		player: server.URL + "/mediainfocombo",
		guide:  server.URL + "/grilles-tv",
		slower: time.Millisecond,
	}
	var err error
	if p.account, err = load(db); err != nil {
		t.Fatal(err)
	}
	return f, p
}

// signedIn returns a fake TF1 and a provider that has an account with it.
func signedIn(t *testing.T) (*tf1, *Provider) {
	t.Helper()
	f, p := serve(t, nil)
	f.tokens = 1
	p.account = &account{Access: "access1", Refresh: "refresh1", Expires: time.Now().Add(time.Hour)}
	return f, p
}

// set changes how the fake behaves while it may be answering.
func (f *tf1) set(change func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	change()
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

	if got, _ := p.Channels(t.Context()); !slices.Equal(got, lci) || p.Login() != (provider.Login{}) {
		t.Fatalf("before signing in: channels %v, sign-in %+v", got, p.Login())
	}
	if err := p.SignIn(t.Context()); err != nil {
		t.Fatal(err)
	}
	login := p.Login()
	if login.State != provider.Pending || login.Code != "1234-5678" || login.URL != f.url+"/tv?user_code=1234-5678" || time.Until(login.Expires) <= 0 {
		t.Fatalf("waiting for the user: %+v, want the code and where to enter it", login)
	}

	f.set(func() { f.code = "" }) // the user enters the code
	<-changes
	if login := p.Login(); login != (provider.Login{State: provider.SignedIn}) {
		t.Errorf("signed in: %+v", login)
	}
	if got, _ := p.Channels(t.Context()); !slices.Equal(got, everything) {
		t.Errorf("Channels() = %v, want %v", got, everything)
	}

	// The account outlasts a restart.
	if _, restarted := serve(t, db); restarted.Login().State != provider.SignedIn {
		t.Errorf("after a restart: %+v, want the account", restarted.Login())
	}

	if err := p.SignOut(); err != nil {
		t.Fatal(err)
	}
	<-changes
	if got, _ := p.Channels(t.Context()); !slices.Equal(got, lci) || p.Login() != (provider.Login{}) {
		t.Errorf("signed out: channels %v, sign-in %+v", got, p.Login())
	}
	if kept, err := load(db); kept != nil || err != nil {
		t.Errorf("signed out, the store still has %+v, %v", kept, err)
	}
}

func TestSignInFails(t *testing.T) {
	for code, want := range map[string]string{
		"expired_token":   codeExpired,
		"access_denied":   "The sign-in was turned down on TF1's site.",
		"invalid_request": "TF1 refused the sign-in: Bad Request.",
	} {
		f, p := signedIn(t)
		f.code = code
		if err := p.SignIn(t.Context()); err != nil {
			t.Fatal(err)
		}
		// The account that was there stays.
		if login := providertest.Await(t, p, provider.SignedIn); login.Problem != want {
			t.Errorf("code answered with %s: %+v, want the problem %q", code, login, want)
		}
	}

	// A code runs out by itself, should TF1 not say so.
	f, p := serve(t, nil)
	f.codeLife = 0.02
	if err := p.SignIn(t.Context()); err != nil {
		t.Fatal(err)
	}
	if login := providertest.Await(t, p, provider.SignedOut); login != (provider.Login{Problem: codeExpired}) {
		t.Errorf("code that ran out: %+v", login)
	}

	// TF1 asks for patience: the next look at the code comes later.
	f, p = serve(t, nil)
	f.code, p.slower = "slow_down", 50*time.Millisecond
	if err := p.SignIn(t.Context()); err != nil {
		t.Fatal(err)
	}
	providertest.Await(t, p, provider.SignedIn)
	if waited := f.polled[1].Sub(f.polled[0]); len(f.polled) != 2 || waited < p.slower {
		t.Errorf("asked about the code %d times, the second %s after the first: want it %s later", len(f.polled), waited, p.slower)
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

	// Signing out while TF1 is still asked for a code leaves no code behind.
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

// TestTokens follows an account's tokens as they run out, are taken back and
// are refused.
func TestTokens(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	f, p := serve(t, db)
	f.tokens = 1
	p.account = &account{Access: "access1", Refresh: "refresh1", Expires: time.Now().Add(-time.Minute)}
	tune := func(channel string) error {
		_, err := p.Stream(t.Context(), channel)
		return err
	}
	age := func(left time.Duration) {
		p.mu.Lock()
		defer p.mu.Unlock()
		p.account = &account{Access: p.account.Access, Refresh: p.account.Refresh, Expires: time.Now().Add(left)}
	}

	// Two channels tuned at once renew a token that has run out once.
	f.slow = 20 * time.Millisecond
	var tuning sync.WaitGroup
	for _, channel := range []string{"tf1", "tfx"} {
		tuning.Go(func() {
			if err := tune(channel); err != nil {
				t.Error(err)
			}
		})
	}
	tuning.Wait()
	if kept, _ := load(db); f.renewals != 1 || kept == nil || kept.Access != "access2" || kept.Refresh != "refresh2" || time.Until(kept.Expires) < 11*time.Hour {
		t.Fatalf("after two tunes with a token that had run out: %d renewals, %+v in the store: want one, and its tokens kept", f.renewals, kept)
	}
	slices.Sort(f.asked)
	if want := []string{"L_TF1 access2", "L_TFX access2"}; !slices.Equal(f.asked, want) {
		t.Errorf("the player's API was asked for %q, want %q", f.asked, want)
	}

	// TF1 takes a token back before its time.
	f.dropped["access2"] = true
	if err := tune("tfx"); err != nil || f.renewals != 2 || p.Login().State != provider.SignedIn {
		t.Errorf("a token TF1 took back: %v after %d renewals, sign-in %+v: want it renewed", err, f.renewals, p.Login())
	}

	// TF1 does not answer for tokens: one that has time left will do.
	f.down = true
	age(time.Minute)
	if err := tune("tfx"); err != nil {
		t.Errorf("with TF1 out of reach and a minute left: %v", err)
	}
	age(-time.Minute)
	if err := tune("tfx"); err == nil || !strings.Contains(err.Error(), "renewing the sign-in") || p.Login().State != provider.SignedIn {
		t.Errorf("with TF1 out of reach and no time left: %v, sign-in %+v: want an error, and the account as it was", err, p.Login())
	}

	// TF1 takes the refresh token no more: the account needs signing in again.
	f.down, f.revoked = false, true
	if err := tune("tfx"); err == nil || !strings.Contains(err.Error(), "sign in again") || p.Login().State != provider.Expired {
		t.Errorf("with the refresh token refused: %v, sign-in %+v: want it expired", err, p.Login())
	}
	if got, _ := p.Channels(t.Context()); !slices.Equal(got, everything) || tune("lci") != nil {
		t.Errorf("with the sign-in expired: channels %v, LCI %v: want the channels kept, and LCI playing", got, tune("lci"))
	}

	// The guide looks after the account as well.
	f.revoked = false
	if _, err := p.Programmes(t.Context(), lci, time.Now(), time.Now().Add(time.Hour)); err != nil || f.renewals != 3 || p.Login().State != provider.SignedIn {
		t.Errorf("after the guide: %v, %d renewals, sign-in %+v: want the tokens renewed and the account accepted", err, f.renewals, p.Login())
	}

	// A viewer who leaves while the tokens are renewed: TF1 has taken the
	// refresh token, so the new ones are kept all the same.
	age(-time.Minute)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Millisecond)
	defer cancel()
	_, err = p.Stream(ctx, "tfx")
	if kept, _ := load(db); err == nil || f.renewals != 4 || kept == nil || kept.Access != "access5" {
		t.Errorf("tune given up during a renewal: %v, %d renewals, %+v in the store: want the tune failed and the new tokens kept", err, f.renewals, kept)
	}
	if err := tune("tfx"); err != nil || f.renewals != 4 {
		t.Errorf("the tune after: %v with %d renewals, want it to go with the tokens kept", err, f.renewals)
	}
}

func TestStream(t *testing.T) {
	f, p := signedIn(t)
	for _, stream := range [][2]string{
		{"tf1", "/pass1/prod/TF1/cmaf/out/TF1.m3u8"}, // as it is broadcast
		{"tf1-series-films", "/pass2/prod/TF1-SERIES-FILMS/cmaf/out/TF1-SERIES-FILMS.m3u8"},
		{"lci", "/pass3/prod/LCI/cmaf/out/LCI.m3u8"},
	} {
		if source, err := p.Stream(t.Context(), stream[0]); err != nil || source.URL != f.url+stream[1] {
			t.Errorf("Stream(%s) = %+v, %v: want the master playlist at %s", stream[0], source, err, stream[1])
		}
	}
	slices.Sort(f.asked)
	// LCI takes no account, so none is named.
	if want := []string{"L_LCI", "L_TF1 access1", "L_TF1-SERIES-FILMS access1"}; !slices.Equal(f.asked, want) {
		t.Errorf("the player's API was asked for %q, want %q", f.asked, want)
	}

	for refusal, want := range map[string]string{
		"GEOBLOCKED":        "-tf1-proxy",
		"PERMISSION_DENIED": "no access to TFX",
		"NOT_FOUND":         "TFX is unavailable: TF1 answers 403 NOT_FOUND",
	} {
		f.refusal = refusal
		if _, err := p.Stream(t.Context(), "tfx"); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Stream() refused with %s = %v, want an error with %q", refusal, err, want)
		}
	}
	f.refusal, f.drm = "", true
	if _, err := p.Stream(t.Context(), "tfx"); err == nil || !strings.Contains(err.Error(), "DRM") {
		t.Errorf("Stream() of an encrypted channel = %v, want a DRM error", err)
	}
	f.drm, f.format = false, "dash"
	if _, err := p.Stream(t.Context(), "tfx"); err == nil || !strings.Contains(err.Error(), "no HLS stream") {
		t.Errorf("Stream() of a channel in another format = %v, want an error that says so", err)
	}
	if login := p.Login(); login != (provider.Login{State: provider.SignedIn}) {
		t.Errorf("sign-in after refusals that say nothing of it: %+v", login)
	}

	_, signedOut := serve(t, nil)
	if _, err := signedOut.Stream(t.Context(), "tfx"); err == nil || !strings.Contains(err.Error(), "sign in") {
		t.Errorf("Stream() without an account = %v, want a call to sign in", err)
	}
	if _, err := signedOut.Stream(t.Context(), "lci"); err != nil {
		t.Errorf("Stream() of LCI without an account = %v", err)
	}
	if source, err := signedOut.Stream(t.Context(), "tmc"); err == nil {
		t.Errorf("Stream() of a channel that is not offered = %+v, want an error", source)
	}
}

// TestPass follows a stream through the client it is read with, as its pass
// runs out.
func TestPass(t *testing.T) {
	f, p := signedIn(t)
	source, err := p.Stream(t.Context(), "tfx")
	if err != nil {
		t.Fatal(err)
	}
	segments, release := httpclient.SegmentClient(source.Client)
	defer release()
	// As ffmpeg asks: always with the pass the stream started with.
	fetch := func(client *http.Client, file string) (status int, body, from string) {
		t.Helper()
		resp, body := providertest.Get(t, client, f.url+"/pass1/prod/TFX/cmaf/out/"+file)
		return resp.StatusCode, body, resp.Request.URL.Path
	}

	if status, body, _ := fetch(source.Client, "high.m3u8"); status != http.StatusOK || body != "TFX/high.m3u8" || f.passes != 1 {
		t.Errorf("a playlist: %d %q with %d passes handed out", status, body, f.passes)
	}
	// The pass runs out: the stream goes on with a new one.
	f.set(func() { f.expired["pass1"] = true })
	status, _, from := fetch(source.Client, "high.m3u8")
	if status != http.StatusOK || f.passes != 2 {
		t.Errorf("after the pass ran out: %d with %d passes handed out, want 200 with a second", status, f.passes)
	}
	// What the playlist lists is found from where the playlist is, and so
	// asked for with the first pass too.
	if want := "/pass1/prod/TFX/cmaf/out/high.m3u8"; from != want {
		t.Errorf("after the pass ran out, the playlist is from %s, want it from where it was asked for, %s", from, want)
	}
	if status, body, _ := fetch(segments, "high-1.mp4"); status != http.StatusOK || body != "TFX/high-1.mp4" || f.passes != 2 {
		t.Errorf("a segment after: %d %q with %d passes handed out, want it with the second", status, body, f.passes)
	}
	// A segment is the first to be refused as well.
	f.set(func() { f.expired["pass2"] = true })
	if status, _, _ := fetch(segments, "high-2.mp4"); status != http.StatusOK || f.passes != 3 {
		t.Errorf("a segment after the second pass ran out: %d with %d passes handed out, want 200 with a third", status, f.passes)
	}

	// An address outside France is refused with any pass.
	f.set(func() { f.abroad = true })
	if status, body, _ := fetch(source.Client, "high.m3u8"); status != http.StatusForbidden || !strings.Contains(body, "geoip") || f.passes != 3 {
		t.Errorf("outside France: %d %q with %d passes handed out, want TF1's refusal and no pass more", status, body, f.passes)
	}
	// A new pass that is refused is refused for something else.
	f.set(func() { f.abroad, f.expired["pass3"] = false, true })
	p.rest = time.Hour
	again, err := p.Stream(t.Context(), "tfx")
	if err != nil {
		t.Fatal(err)
	}
	f.set(func() { f.expired["pass4"] = true })
	resp, _ := providertest.Get(t, again.Client, f.url+"/pass4/prod/TFX/cmaf/out/high.m3u8")
	if resp.StatusCode != http.StatusForbidden || f.passes != 4 {
		t.Errorf("a new pass refused: %s with %d passes handed out, want 403 and no pass more", resp.Status, f.passes)
	}
}

// TestBroadcastAcrossTheClockChange reads the night the clocks go forward:
// the times past midnight are of the day after, on which they exist.
func TestBroadcastAcrossTheClockChange(t *testing.T) {
	page := row("23:00", "Film", "", "", "/f.jpg") + row("02:30", "Programmes de nuit", "", "", "/n.jpg") + row("05:50", "TFou", "", "", "/t.jpg")
	var got []string
	for _, programme := range broadcast(page, time.Date(2026, 3, 29, 0, 0, 0, 0, paris), "https://guide.example/x") {
		got = append(got, programme.Start.UTC().Format("01-02 15:04")+" "+programme.Title)
	}
	want := []string{"03-29 21:00 Film", "03-30 00:30 Programmes de nuit", "03-30 03:50 TFou"}
	if !slices.Equal(got, want) {
		t.Errorf("broadcast() = %q, want %q", got, want)
	}
}

// row is a programme of a day of the guide, as TF1 writes it.
func row(clock, title, episode, about, picture string) string {
	return `<div role="listitem" class="views-row odd"><div class="donnees-jour">
<a href="/programmes/diffusion/2026-10-07-0550-tfou-mercredi" hreflang="fr" class="use-ajax">
	<div class="heure">    ` + clock + `
</div>
<div class="views-field views-field-field-diaporama"><div class="field-content"><picture>
	<source srcset="` + picture + ` 1x" media="(max-width: 575.98px)" type="image/jpeg" width="595" height="335"/>
	<img loading="lazy" width="175" height="100" src="/small.jpg" alt="" />
</picture></div></div>	<div class="titre-genre">
		<h5 >
			` + title + `
		</h5>
		<div class="episode">
			` + episode + `
		</div>
		<div class="complements"><span>Série</span><span>In&eacute;dit</span></div>
	</div>
	<div class="description">
		<div class="generique"><p>Présenté par <strong>Bruce Toussaint</strong></p></div>
		<div class="resume"><p>` + about + `</p>
</div>
	</div>
</a>
</div>
</div>
`
}

func TestProgrammes(t *testing.T) {
	f, p := serve(t, nil)
	night := row("00:35", "Programmes de nuit", "", "Retrouvez tous vos programmes de nuit.", "/sites/nuit.jpg")
	f.pages = map[string]string{
		"TF1 2026-10-06": row("21:10", "Tracker", "", "", "/sites/tracker.jpg") + night,
		"TF1 2026-10-07": row("05:50", "TFou", "", "Molang / Dora (ST)", "/sites/default/files/styles/536_x_302/public/Tfou.jpg?itok=AB&amp;h=1") +
			row("06:55", "Bonjour !", "La matinale TF1", "Le premier rendez-vous de la journée.", "/sites/bonjour.jpg") +
			row("21:10", "Les feux de l&#039;amour", "", "", "/sites/feux.jpg") + night,
		"TF1 2026-10-08": row("05:50", "TFou", "", "Molang / Dora (ST)", "/sites/tfou.jpg"),
	}
	// From four in the morning in Paris, where it is summer.
	from := time.Date(2026, 10, 7, 2, 0, 0, 0, time.UTC)
	to := from.Add(24 * time.Hour)
	programmes, err := p.Programmes(t.Context(), []provider.Channel{everything[0], lci[0]}, from, to)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, programme := range programmes {
		got = append(got, fmt.Sprintf("%s %s to %s: %s | %s | %s", programme.ChannelID,
			programme.Start.UTC().Format("02 15:04"), programme.Stop.UTC().Format("02 15:04"),
			programme.Title, strings.ReplaceAll(programme.Description, "\n", " / "), strings.TrimPrefix(programme.Image, f.url)))
	}
	want := []string{
		// The night of the day before, which the day before lists: until the
		// first programme of the next day.
		"tf1 06 22:35 to 07 03:50: Programmes de nuit | Retrouvez tous vos programmes de nuit. | /sites/nuit.jpg",
		"tf1 07 03:50 to 07 04:55: TFou | Molang / Dora (ST) | /sites/default/files/styles/536_x_302/public/Tfou.jpg?itok=AB&h=1",
		"tf1 07 04:55 to 07 19:10: Bonjour ! | La matinale TF1 / Le premier rendez-vous de la journée. | /sites/bonjour.jpg",
		"tf1 07 19:10 to 07 22:35: Les feux de l'amour |  | /sites/feux.jpg",
		"tf1 07 22:35 to 08 03:50: Programmes de nuit | Retrouvez tous vos programmes de nuit. | /sites/nuit.jpg",
	}
	for hour := range 24 {
		at := from.Add(time.Duration(hour) * time.Hour)
		want = append(want, fmt.Sprintf("lci %s to %s: LCI | %s | ", at.Format("02 15:04"), at.Add(time.Hour).Format("02 15:04"), unlisted))
	}
	if !slices.Equal(got, want) {
		t.Errorf("Programmes() =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if want := []string{"TF1 2026-10-06", "TF1 2026-10-07", "TF1 2026-10-08"}; !slices.Equal(f.days, want) {
		t.Errorf("asked for the guide of %q, want %q", f.days, want)
	}

	// A guide with a channel missing would wipe what Plex has of it.
	if _, err := p.Programmes(t.Context(), everything, from, to); err == nil || !strings.Contains(err.Error(), "nothing on TFX") {
		t.Errorf("Programmes() with nothing listed for TFX = %v, want an error that says so", err)
	}
	f.closed = true
	if _, err := p.Programmes(t.Context(), everything[:1], from, to); err == nil || !strings.Contains(err.Error(), "503") {
		t.Errorf("Programmes() with the guide closed = %v, want an error with TF1's answer", err)
	}
}
