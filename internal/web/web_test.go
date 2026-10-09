package web

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/combor/telesfor/internal/provider"
	"github.com/combor/telesfor/internal/tuner"
)

// fake is a provider with two channels.
type fake struct{}

func (fake) Name() string { return "fake" }

func (fake) Channels(context.Context) ([]provider.Channel, error) {
	return []provider.Channel{{ID: "one", Name: "One"}, {ID: "two", Name: "Two & <more>"}}, nil
}

func (fake) Programmes(context.Context, []provider.Channel, time.Time, time.Time) ([]provider.Programme, error) {
	return nil, nil
}

func (fake) Stream(context.Context, string) (provider.Source, error) {
	return provider.Source{}, nil
}

// club is a provider with an account, and a channel for whoever has signed in.
type club struct {
	fake
	login   provider.Login
	changed func()
	closed  bool // it cannot list its channels
}

func (*club) Name() string { return "club" }

func (c *club) Channels(context.Context) ([]provider.Channel, error) {
	if c.closed {
		return nil, errors.New("closed")
	}
	if c.login.State != provider.SignedIn {
		return nil, nil
	}
	return []provider.Channel{{ID: "members", Name: "Members"}}, nil
}

func (c *club) SignIn(context.Context) error {
	c.login = provider.Login{State: provider.Pending, Code: "ABCDEFGH", URL: "https://club.example/activate?code=ABCDEFGH", Expires: time.Now().Add(5 * time.Minute)}
	return nil
}

func (c *club) SignOut() error {
	c.login = provider.Login{}
	c.changed()
	return nil
}

func (c *club) Login() provider.Login { return c.login }

func (c *club) OnChange(changed func()) { c.changed = changed }

// newUI serves the interface beside the tuners of the fake provider and of
// the club, the way main does.
func newUI(t *testing.T) (*httptest.Server, *club) {
	t.Helper()
	members := &club{}
	mux := http.NewServeMux()
	ui := &Handler{
		Version:  "1.2.3",
		Settings: []Setting{{Name: "Listen address", Value: ":5004", Flag: "-listen", Env: "TELESFOR_LISTEN"}},
	}
	for _, source := range []struct {
		provider.Provider
		tuner.Device
		settings []Setting
	}{
		{fake{}, tuner.Device{ID: "0BADCAFE", Name: "Fake", First: 1},
			[]Setting{{Name: "Proxy", State: "Not set", Flag: "-fake-proxy", Env: "TELESFOR_FAKE_PROXY"}}},
		{members, tuner.Device{ID: "0BADCAFF", Name: "Club", Path: "/club", First: 1001}, nil},
	} {
		lineup, err := tuner.New(t.Context(), source.Provider, nil, source.Device, nil)
		if err != nil {
			t.Fatal(err)
		}
		lineup.Register(mux)
		ui.Providers = append(ui.Providers, Provider{Tuner: lineup, Settings: source.settings})
	}
	ui.Register(mux)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server, members
}

type response struct {
	status int
	header http.Header
	body   string
}

// get fetches a path without following redirects. header is pairs of a name
// and its value.
func get(t *testing.T, server *httptest.Server, path string, header ...string) response {
	t.Helper()
	return request(t, server, http.MethodGet, path, "", header...)
}

// post posts the way get fetches.
func post(t *testing.T, server *httptest.Server, path string, header ...string) response {
	t.Helper()
	return request(t, server, http.MethodPost, path, "", header...)
}

func request(t *testing.T, server *httptest.Server, method, path, body string, header ...string) response {
	t.Helper()
	req, err := http.NewRequest(method, server.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i+1 < len(header); i += 2 {
		if header[i] == "Host" {
			req.Host = header[i+1]
		}
		req.Header.Set(header[i], header[i+1])
	}
	client := *server.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	answer, _ := io.ReadAll(resp.Body)
	return response{resp.StatusCode, resp.Header, string(answer)}
}

// lacks returns the wants that body does not contain.
func lacks(body string, wants ...string) []string {
	var missing []string
	for _, want := range wants {
		if !strings.Contains(body, want) {
			missing = append(missing, want)
		}
	}
	return missing
}

func TestTabs(t *testing.T) {
	server, _ := newUI(t)
	if r := get(t, server, "/"); r.status != http.StatusSeeOther || r.header.Get("Location") != "/ui/" {
		t.Errorf("/ = %d to %q, want a redirect to /ui/", r.status, r.header.Get("Location"))
	}
	if r := get(t, server, "/lineup.json"); r.status != http.StatusOK {
		t.Errorf("the tuner's lineup.json beside the interface = %d", r.status)
	}
	// A page left open since before the tabs refreshes from here.
	if r := get(t, server, "/ui/settings"); r.status != http.StatusSeeOther || r.header.Get("Location") != "/ui/" {
		t.Errorf("/ui/settings = %d to %q, want a redirect to /ui/", r.status, r.header.Get("Location"))
	}
	if r := get(t, server, "/ui/providers/nobody"); r.status != http.StatusNotFound {
		t.Errorf("the tab of a provider there is none of = %d, want 404", r.status)
	}

	// The page opens on the first provider's tab.
	for _, path := range []string{"/ui/", "/ui/providers/fake"} {
		r := get(t, server, path)
		if missing := lacks(r.body,
			"<title>Fake — telesfor</title>",
			`<h1 class="page-title">Settings</h1>`,
			`hx-get="/ui/providers/fake" hx-trigger="every 5s"`,
			`<header class="topbar" data-state="idle">`,
			`<span class="version">v1.2.3</span>`,
			`<a class="tab" href="/ui/providers/fake" aria-current="page">Fake</a>`,
			`<a class="tab" href="/ui/providers/club">Club</a>`,
			`<a class="tab" href="/ui/server">Server</a>`,
			// The addresses are where the page reached telesfor.
			`<code class="value">`+server.URL+`</code>`,
			`<code class="value">`+server.URL+`/xmltv.xml</code>`,
			`>Startup settings</h2>`,
			`<dd>Not set<p class="for"><code>-fake-proxy</code> or <code>TELESFOR_FAKE_PROXY</code></p>`,
			`<span class="count">2</span>`,
			`<span class="pos">1</span>`,
			"Two &amp; &lt;more&gt;",
		); r.status != http.StatusOK || missing != nil {
			t.Errorf("%s = %d, lacks %q:\n%s", path, r.status, missing, r.body)
		}
		// Nothing of the other tabs, or of what the provider has none of.
		for _, other := range []string{server.URL + "/club", ">Account</h2>", "TELESFOR_LISTEN"} {
			if strings.Contains(r.body, other) {
				t.Errorf("%s shows %q", path, other)
			}
		}
		if csp := r.header.Get("Content-Security-Policy"); !strings.HasPrefix(csp, "default-src 'self'") {
			t.Errorf("%s has the content security policy %q", path, csp)
		}
	}

	r := get(t, server, "/ui/providers/club")
	if missing := lacks(r.body,
		"<title>Club — telesfor</title>",
		`hx-get="/ui/providers/club" hx-trigger="every 5s"`,
		`<a class="tab" href="/ui/providers/club" aria-current="page">Club</a>`,
		`<code class="value">`+server.URL+`/club</code>`,
		`<code class="value">`+server.URL+`/club/xmltv.xml</code>`,
		`>Account</h2>`,
		`<span class="badge">Signed out</span>`,
		`<form method="post" action="/ui/providers/club/sign-in"><button class="button button-primary">Sign in</button></form>`,
		"Club has channels for an account.",
	); r.status != http.StatusOK || missing != nil || strings.Contains(r.body, "Startup settings") {
		t.Errorf("the club's tab = %d, lacks %q, or shows startup settings it has none of:\n%s", r.status, missing, r.body)
	}

	r = get(t, server, "/ui/server")
	if missing := lacks(r.body,
		"<title>Server — telesfor</title>",
		`hx-get="/ui/server" hx-trigger="every 5s"`,
		`<a class="tab" href="/ui/server" aria-current="page">Server</a>`,
		"<dd>v1.2.3</dd>",
		`<code class="value">:5004</code><p class="for"><code>-listen</code> or <code>TELESFOR_LISTEN</code></p>`,
	); r.status != http.StatusOK || missing != nil || strings.Contains(r.body, "Connect Plex") {
		t.Errorf("the server's tab = %d, lacks %q, or shows a provider's part:\n%s", r.status, missing, r.body)
	}

	// Asked for by a name, the addresses follow it. A reverse proxy asks by a
	// name of its own: the addresses are then where the proxy found telesfor.
	if r := get(t, server, "/ui/", "Host", "telesfor.example:5004"); !strings.Contains(r.body, `<code class="value">http://telesfor.example:5004/xmltv.xml</code>`) {
		t.Errorf("tab asked for by a name lacks the name in its addresses:\n%s", r.body)
	}
	r = get(t, server, "/ui/providers/club", "Host", "telesfor.example", "X-Forwarded-For", "192.0.2.7")
	if missing := lacks(r.body, `<code class="value">`+server.URL+`/club</code>`, `<code class="value">`+server.URL+`/club/xmltv.xml</code>`); missing != nil ||
		strings.Contains(r.body, "telesfor.example") {
		t.Errorf("tab asked for through a proxy lacks %q, or has the proxy's name:\n%s", missing, r.body)
	}

	// A refresh gets the part of the page that changes, not a second page.
	r = get(t, server, "/ui/providers/club", "HX-Request", "true")
	if missing := lacks(r.body, "<title>Club — telesfor</title>", `<span class="pill-label">Idle</span>`, `>Account</h2>`); missing != nil ||
		strings.Contains(r.body, "<html") {
		t.Errorf("refreshed tab lacks %q, or is a whole page:\n%s", missing, r.body)
	}
}

// TestSignIn follows the club's sign-in on its tab, from asking for a code to
// signing out again.
func TestSignIn(t *testing.T) {
	server, members := newUI(t)
	tab := func() string { return get(t, server, "/ui/providers/club").body }

	// A page of another site must not start it.
	if r := post(t, server, "/ui/providers/club/sign-in", "Sec-Fetch-Site", "cross-site"); r.status != http.StatusForbidden || members.login.State != provider.SignedOut {
		t.Errorf("sign-in from another site = %d, sign-in %+v: want it refused", r.status, members.login)
	}
	if r := post(t, server, "/ui/providers/fake/sign-in"); r.status != http.StatusNotFound {
		t.Errorf("sign-in to a provider without accounts = %d, want 404", r.status)
	}

	if r := post(t, server, "/ui/providers/club/sign-in", "Sec-Fetch-Site", "same-origin"); r.status != http.StatusSeeOther || r.header.Get("Location") != "/ui/providers/club" {
		t.Fatalf("sign-in = %d to %q, want a redirect to the tab", r.status, r.header.Get("Location"))
	}
	if missing := lacks(tab(),
		`<span class="badge">Waiting for the code</span>`,
		`<a href="https://club.example/activate?code=ABCDEFGH" target="_blank" rel="noopener noreferrer">club.example/activate</a>`,
		`<code class="value code">ABCDEFGH</code>`,
		"The code works for another 5 minutes.",
		`<form method="post" action="/ui/providers/club/sign-out"><button class="button">Cancel</button></form>`,
	); missing != nil {
		t.Errorf("tab with a code to enter lacks %q", missing)
	}

	// The user enters the code: the club's channel joins its tuner.
	members.login = provider.Login{State: provider.SignedIn}
	members.changed()
	if missing := lacks(tab(),
		`<span class="badge badge-ok">Signed in</span>`,
		`<span class="count">1</span>`,
		`<span class="pos">1001</span>`,
		`<button class="button">Sign out</button>`,
	); missing != nil {
		t.Errorf("tab of a signed-in club lacks %q", missing)
	}

	members.login = provider.Login{State: provider.Expired, Problem: "The code expired before it was entered."}
	if missing := lacks(tab(),
		`<span class="badge badge-warn">Sign-in expired</span>`,
		`<button class="button button-primary">Sign in again</button>`,
		`<p class="problem" role="status">The code expired before it was entered.</p>`,
	); missing != nil {
		t.Errorf("tab of an expired sign-in with a problem lacks %q", missing)
	}
	// Every other tab says so too.
	if missing := lacks(get(t, server, "/ui/server").body,
		`<a class="tab" href="/ui/providers/club" title="Sign-in expired">Club<span class="dot" aria-hidden="true"></span></a>`,
	); missing != nil {
		t.Errorf("another tab, with the club's sign-in expired, lacks %q", missing)
	}

	if r := post(t, server, "/ui/providers/club/sign-out"); r.status != http.StatusSeeOther || r.header.Get("Location") != "/ui/providers/club" {
		t.Fatalf("sign-out = %d to %q, want a redirect to the tab", r.status, r.header.Get("Location"))
	}
	if missing := lacks(tab(), `<span class="badge">Signed out</span>`, `<span class="count">0</span>`, `>Club</a>`); missing != nil {
		t.Errorf("tab after signing out lacks %q", missing)
	}
}

// TestDisable disables the fake provider on its tab and enables it through
// the API, then tries the same on a club that cannot list its channels.
func TestDisable(t *testing.T) {
	server, members := newUI(t)
	patch := func(path, body string, header ...string) response {
		t.Helper()
		return request(t, server, http.MethodPatch, path, body, header...)
	}

	// A page of another site must not do it.
	if r := post(t, server, "/ui/providers/fake/disable", "Sec-Fetch-Site", "cross-site"); r.status != http.StatusForbidden {
		t.Errorf("disabling from another site = %d, want it refused", r.status)
	}
	if r := patch("/api/providers/fake", `{"enabled": false}`, "Sec-Fetch-Site", "cross-site"); r.status != http.StatusForbidden {
		t.Errorf("disabling through the API from another site = %d, want it refused", r.status)
	}
	if r := get(t, server, "/api/providers"); r.status != http.StatusOK || r.header.Get("Content-Type") != "application/json" ||
		r.body != `[{"id":"fake","name":"Fake","enabled":true},{"id":"club","name":"Club","enabled":true}]`+"\n" {
		t.Errorf("the API's providers = %d %q: %s", r.status, r.header.Get("Content-Type"), r.body)
	}

	if r := post(t, server, "/ui/providers/fake/disable"); r.status != http.StatusSeeOther || r.header.Get("Location") != "/ui/providers/fake" {
		t.Fatalf("disabling = %d to %q, want a redirect to the tab", r.status, r.header.Get("Location"))
	}
	r := get(t, server, "/ui/providers/fake")
	if missing := lacks(r.body,
		`<a class="tab tab-disabled" href="/ui/providers/fake" aria-current="page" title="Disabled">Fake</a>`,
		`<span class="badge">Disabled</span>`,
		`<form method="post" action="/ui/providers/fake/enable"><button class="button button-primary">Enable</button></form>`,
	); missing != nil {
		t.Errorf("tab of a disabled provider lacks %q:\n%s", missing, r.body)
	}
	for _, gone := range []string{"Connect Plex", "Startup settings", ">Channels</h2>", `role="status"`} {
		if strings.Contains(r.body, gone) {
			t.Errorf("tab of a disabled provider shows %q", gone)
		}
	}
	if r := get(t, server, "/lineup.json"); r.status != http.StatusNotFound {
		t.Errorf("lineup.json of a disabled provider = %d, want 404", r.status)
	}
	// The page opens on the first tab there is something to see on.
	if r := get(t, server, "/ui/"); !strings.Contains(r.body, "<title>Club — telesfor</title>") {
		t.Errorf("the page, with its first provider disabled, does not open on the next:\n%s", r.body)
	}

	for body, want := range map[string]int{``: http.StatusBadRequest, `{"enable": true}`: http.StatusBadRequest, `{"enabled": true}`: http.StatusOK} {
		if r := patch("/api/providers/fake", body); r.status != want {
			t.Errorf("PATCH with the body %q = %d, want %d", body, r.status, want)
		}
	}
	if r := patch("/api/providers/nobody", `{"enabled": true}`); r.status != http.StatusNotFound {
		t.Errorf("PATCH of a provider there is none of = %d, want 404", r.status)
	}
	if r := patch("/api/providers/fake", `{"enabled": true}`); r.body != `{"id":"fake","name":"Fake","enabled":true}`+"\n" {
		t.Errorf("PATCH that changes nothing answers %q", r.body)
	}
	if missing := lacks(get(t, server, "/ui/").body,
		`<a class="tab" href="/ui/providers/fake" aria-current="page">Fake</a>`,
		`<span class="badge badge-ok">Enabled</span>`,
		`<form method="post" action="/ui/providers/fake/disable"><button class="button">Disable</button></form>`,
		`<span class="count">2</span>`,
	); missing != nil {
		t.Errorf("tab of a provider enabled again lacks %q", missing)
	}

	// A provider that cannot list its channels stays disabled, and its tab
	// says so until it can.
	if r := patch("/api/providers/club", `{"enabled": false}`); r.body != `{"id":"club","name":"Club","enabled":false}`+"\n" {
		t.Fatalf("PATCH that disables answers %d %q", r.status, r.body)
	}
	members.closed = true
	if r := patch("/api/providers/club", `{"enabled": true}`); r.status != http.StatusInternalServerError {
		t.Errorf("enabling a provider that cannot list its channels = %d, want 500", r.status)
	}
	post(t, server, "/ui/providers/club/enable")
	tab := get(t, server, "/ui/providers/club").body
	if missing := lacks(tab, `<span class="badge">Disabled</span>`, `<p class="problem" role="status">Club could not be enabled. telesfor’s log has the reason.</p>`); missing != nil ||
		strings.Contains(tab, ">Account</h2>") {
		t.Errorf("tab of a provider that could not be enabled lacks %q, or shows its account:\n%s", missing, tab)
	}
	members.closed = false
	post(t, server, "/ui/providers/club/enable")
	if tab := get(t, server, "/ui/providers/club").body; !strings.Contains(tab, ">Account</h2>") || strings.Contains(tab, `role="status"`) {
		t.Errorf("tab of a provider enabled at last lacks its account, or still shows what went wrong:\n%s", tab)
	}
}

func TestOnAir(t *testing.T) {
	tabs := []tab{
		{Name: "Fake", Path: "/ui/providers/fake", Current: true},
		{Name: "Club", Path: "/ui/providers/club"},
	}
	lineup := []tuner.Station{{Number: "1", Name: "One"}, {Number: "2", Name: "Two", Streams: 1}, {Number: "3", Name: "Three", Streams: 2}}
	var page bytes.Buffer
	if err := providerPage.ExecuteTemplate(&page, "layout", providerView{frame: frame{Version: "dev", Tabs: tabs, OnAir: true}, Name: "Fake", Channels: lineup}); err != nil {
		t.Fatal(err)
	}
	if missing := lacks(page.String(),
		`<header class="topbar" data-state="on-air">`,
		`<span class="pill pill-on-air">`,
		`<span class="pill-label">On air</span>`,
		`<span class="version">dev</span>`,
		`<span class="pos">1</span>`,
		`<span class="pos pos-ok">2</span>`,
		`<span class="badge badge-ok">On air</span>`,
		`<span class="badge badge-ok">On air ×2</span>`,
	); missing != nil {
		t.Errorf("tab with channels on air lacks %q:\n%s", missing, page.String())
	}
	if on := strings.Count(page.String(), "channel-on-air"); on != 2 {
		t.Errorf("%d channels are shown on air, want 2", on)
	}

	page.Reset()
	if err := providerPage.ExecuteTemplate(&page, "layout", providerView{Name: "Fake"}); err != nil {
		t.Fatal(err)
	}
	if missing := lacks(page.String(), `<header class="topbar" data-state="idle">`, `<span class="count">0</span>`, "Fake offered no channels."); missing != nil {
		t.Errorf("tab without channels lacks %q:\n%s", missing, page.String())
	}

	page.Reset()
	if err := serverPage.ExecuteTemplate(&page, "layout", serverView{}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(page.String(), "Version") {
		t.Errorf("server's tab without a version shows one:\n%s", page.String())
	}
}

func TestStaticFiles(t *testing.T) {
	server, _ := newUI(t)
	r := get(t, server, "/ui/static/htmx-4.0.0.min.js")
	if r.status != http.StatusOK || !strings.HasPrefix(r.header.Get("Content-Type"), "text/javascript") ||
		r.header.Get("X-Content-Type-Options") != "nosniff" || !strings.Contains(r.body, "htmx") {
		t.Fatalf("htmx = %d %v", r.status, r.header)
	}
	tag := r.header.Get("ETag")
	if r := get(t, server, "/ui/static/htmx-4.0.0.min.js", "If-None-Match", tag); tag == "" || r.status != http.StatusNotModified {
		t.Errorf("revalidating ETag %q = %d, want 304", tag, r.status)
	}
	for _, name := range []string{"style.css", "icon.svg"} {
		if r := get(t, server, "/ui/static/"+name); r.status != http.StatusOK {
			t.Errorf("%s = %d", name, r.status)
		}
	}
}
