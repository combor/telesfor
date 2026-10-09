// Package web serves the browser interface: a settings page with a tab for
// each provider, which holds what to enter in Plex for it, its sign-in, its
// settings, its channels and the switch that disables it, and a tab for the
// settings telesfor was started with.
//
// It also serves the API, which lists the providers and enables or disables
// them.
//
// static/htmx-4.0.0.min.js is dist/htmx.min.js from the htmx.org 4.0.0 npm
// package, under the Zero-Clause BSD license.
package web

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"sync"

	"github.com/combor/telesfor/internal/tuner"
)

//go:embed templates static
var files embed.FS

var (
	layout       = template.Must(template.ParseFS(files, "templates/layout.html"))
	providerPage = page("templates/provider.html")
	serverPage   = page("templates/server.html")
)

func page(name string) *template.Template {
	return template.Must(template.Must(layout.Clone()).ParseFS(files, name))
}

// Handler serves the interface under /ui/ and the API under /api/. Like the
// tuners they are open to whoever can reach them, and all they let them
// change is a provider's sign-in, and whether it is enabled.
type Handler struct {
	Providers []Provider // a tab for each
	Settings  []Setting  // telesfor's own, as it was started with
	Version   string

	failed sync.Map // the providers that could not be enabled or disabled when last asked, by name
}

// Provider is a provider's tuner, and the settings of the provider that
// telesfor was started with, such as its proxy.
type Provider struct {
	Tuner    *tuner.Tuner
	Settings []Setting
}

// Register adds the routes of the interface and of the API to mux.
func (h *Handler) Register(mux *http.ServeMux) {
	// A page of another site must not sign anyone in or out, or disable a
	// provider.
	sameOrigin := http.NewCrossOriginProtection()
	mux.Handle("GET /{$}", http.RedirectHandler("/ui/", http.StatusSeeOther))
	mux.Handle("GET /ui/{$}", secure(http.HandlerFunc(h.providerTab)))
	mux.Handle("GET /ui/providers/{provider}", secure(http.HandlerFunc(h.providerTab)))
	mux.Handle("GET /ui/server", secure(http.HandlerFunc(h.serverTab)))
	// Where the page was before it had tabs: one left open still refreshes from there.
	mux.Handle("GET /ui/settings", http.RedirectHandler("/ui/", http.StatusSeeOther))
	mux.Handle("POST /ui/providers/{provider}/sign-in", sameOrigin.Handler(http.HandlerFunc(h.signIn)))
	mux.Handle("POST /ui/providers/{provider}/sign-out", sameOrigin.Handler(http.HandlerFunc(h.signOut)))
	mux.Handle("POST /ui/providers/{provider}/enable", sameOrigin.Handler(h.switchTo(true)))
	mux.Handle("POST /ui/providers/{provider}/disable", sameOrigin.Handler(h.switchTo(false)))
	mux.HandleFunc("GET /api/providers", h.listProviders)
	mux.Handle("PATCH /api/providers/{provider}", sameOrigin.Handler(http.HandlerFunc(h.patchProvider)))
	mux.Handle("GET /ui/static/", secure(http.StripPrefix("/ui/static/", staticFiles())))
}

// Allow only the interface's own scripts, styles and images.
func secure(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'self'; frame-ancestors 'none'; form-action 'self'")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}

// Embedded files have no modification time, so tag them by content for
// revalidation.
func staticFiles() http.Handler {
	static, err := fs.Sub(files, "static")
	if err != nil {
		panic(err)
	}
	tags := make(map[string]string)
	err = fs.WalkDir(static, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(static, path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(b)
		tags[path] = `"` + base64.RawURLEncoding.EncodeToString(sum[:12]) + `"`
		return nil
	})
	if err != nil {
		panic(err)
	}
	serve := http.FileServerFS(static)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if tag, ok := tags[r.URL.Path]; ok {
			w.Header().Set("ETag", tag)
			w.Header().Set("Cache-Control", "no-cache")
		}
		serve.ServeHTTP(w, r)
	})
}

func htmx(r *http.Request) bool { return r.Header.Get("HX-Request") == "true" }

// render shows a tab: the whole page, or only its refreshing part for htmx.
// It renders fully before writing so a template error cannot send half a page.
func render(w http.ResponseWriter, r *http.Request, t *template.Template, data any) {
	name := "layout"
	if htmx(r) {
		name = "update"
	}
	w.Header().Add("Vary", "HX-Request")
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, name, data); err != nil {
		slog.Error("rendering a page", "template", name, "err", err)
		http.Error(w, "can't show the page", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	buf.WriteTo(w)
}

// Release builds set a bare version number.
func displayVersion(s string) string {
	if s != "" && s[0] >= '0' && s[0] <= '9' {
		return "v" + s
	}
	return s
}
