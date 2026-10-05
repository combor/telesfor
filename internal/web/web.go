// Package web serves the browser interface: a settings page with what to
// enter in Plex for each provider, the providers' sign-ins and channels, and
// the settings telesfor was started with.
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

	"github.com/combor/telesfor/internal/tuner"
)

//go:embed templates static
var files embed.FS

var (
	layout       = template.Must(template.ParseFS(files, "templates/layout.html"))
	settingsPage = parse(layout, "templates/settings.html")
)

func parse(base *template.Template, names ...string) *template.Template {
	return template.Must(template.Must(base.Clone()).ParseFS(files, names...))
}

// Handler serves the interface under /ui/. Like the tuners it is open to
// whoever can reach it, and all it lets them change is a provider's sign-in.
type Handler struct {
	Tuners   []*tuner.Tuner // one for each provider
	Settings []Setting      // as telesfor was started with
	Version  string
}

// Register adds the interface's routes to mux.
func (h *Handler) Register(mux *http.ServeMux) {
	// A page of another site must not sign anyone in or out.
	sameOrigin := http.NewCrossOriginProtection()
	mux.Handle("GET /{$}", http.RedirectHandler("/ui/", http.StatusSeeOther))
	mux.Handle("GET /ui/{$}", secure(http.HandlerFunc(h.settings)))
	mux.Handle("GET /ui/settings", secure(http.HandlerFunc(h.settings)))
	mux.Handle("POST /ui/providers/{provider}/sign-in", sameOrigin.Handler(http.HandlerFunc(h.signIn)))
	mux.Handle("POST /ui/providers/{provider}/sign-out", sameOrigin.Handler(http.HandlerFunc(h.signOut)))
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

// Render fully before writing so a template error cannot send half a page.
func render(w http.ResponseWriter, t *template.Template, name string, data any) {
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
