package web

import (
	"bytes"
	"context"
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

// newUI serves the interface beside a tuner with the fake provider's
// channels, the way main does.
func newUI(t *testing.T) *httptest.Server {
	t.Helper()
	lineup, err := tuner.New(t.Context(), []provider.Provider{fake{}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("/", lineup)
	(&Handler{
		Tuner:   lineup,
		Version: "1.2.3",
		Settings: []Setting{
			{Name: "Listen address", Value: ":5004", Flag: "-listen", Env: "TELESFOR_LISTEN"},
			{Name: "TVP proxy", State: "Not set", Flag: "-tvp-proxy", Env: "TELESFOR_TVP_PROXY"},
		},
	}).Register(mux)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
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
	req, err := http.NewRequest(http.MethodGet, server.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	client := *server.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return response{resp.StatusCode, resp.Header, string(body)}
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

func TestSettingsPage(t *testing.T) {
	server := newUI(t)
	if r := get(t, server, "/"); r.status != http.StatusSeeOther || r.header.Get("Location") != "/ui/" {
		t.Errorf("/ = %d to %q, want a redirect to /ui/", r.status, r.header.Get("Location"))
	}
	if r := get(t, server, "/lineup.json"); r.status != http.StatusOK {
		t.Errorf("the tuner's lineup.json beside the interface = %d", r.status)
	}

	for _, path := range []string{"/ui/", "/ui/settings"} {
		r := get(t, server, path)
		if missing := lacks(r.body,
			"<title>Settings — telesfor</title>",
			`<h1 class="page-title">Settings</h1>`,
			`hx-get="/ui/settings" hx-trigger="every 5s"`,
			`<header class="topbar" data-state="idle">`,
			`<span class="version">v1.2.3</span>`,
			// The addresses are the ones the page was asked for at.
			`<code class="value">`+server.URL+`</code>`,
			`<code class="value">`+server.URL+`/xmltv.xml</code>`,
			`<span class="count">2</span>`,
			`<span class="pos">1</span>`,
			"Two &amp; &lt;more&gt;",
			"<dd>v1.2.3</dd>",
			`<code class="value">:5004</code><p class="for"><code>-listen</code> or <code>TELESFOR_LISTEN</code></p>`,
			`<dd>Not set<p class="for"><code>-tvp-proxy</code> or <code>TELESFOR_TVP_PROXY</code></p>`,
		); r.status != http.StatusOK || missing != nil {
			t.Errorf("%s = %d, lacks %q:\n%s", path, r.status, missing, r.body)
		}
		if csp := r.header.Get("Content-Security-Policy"); !strings.HasPrefix(csp, "default-src 'self'") {
			t.Errorf("%s has the content security policy %q", path, csp)
		}
	}

	// A refresh gets the part of the page that changes, not a second page.
	r := get(t, server, "/ui/settings", "HX-Request", "true")
	if missing := lacks(r.body, "<title>Settings — telesfor</title>", `<span class="pill-label">Idle</span>`); missing != nil ||
		strings.Contains(r.body, "<html") {
		t.Errorf("refreshed settings page lacks %q, or is a whole page:\n%s", missing, r.body)
	}
}

func TestSettingsView(t *testing.T) {
	lineup := []tuner.Station{{Number: "1", Name: "One"}, {Number: "2", Name: "Two", Streams: 1}, {Number: "3", Name: "Three", Streams: 2}}
	var page bytes.Buffer
	if err := settingsPage.ExecuteTemplate(&page, "layout", newSettingsView("http://nas:5004", lineup, nil, "dev")); err != nil {
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
		t.Errorf("settings page with channels on air lacks %q:\n%s", missing, page.String())
	}
	if on := strings.Count(page.String(), "channel-on-air"); on != 2 {
		t.Errorf("%d channels are shown on air, want 2", on)
	}

	page.Reset()
	if err := settingsPage.ExecuteTemplate(&page, "layout", newSettingsView("http://nas:5004", nil, nil, "")); err != nil {
		t.Fatal(err)
	}
	if missing := lacks(page.String(), `<span class="count">0</span>`, "The providers offered no channels."); missing != nil ||
		strings.Contains(page.String(), "Version") {
		t.Errorf("settings page without channels or a version lacks %q, or shows a version:\n%s", missing, page.String())
	}
}

func TestStaticFiles(t *testing.T) {
	server := newUI(t)
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
