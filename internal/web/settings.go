package web

import (
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/combor/telesfor/internal/provider"
	"github.com/combor/telesfor/internal/tuner"
)

// Setting is a setting telesfor was started with, and how to change it.
type Setting struct {
	Name  string
	Value string // an address, shown to copy; empty if the setting has none
	State string // shown in place of a value, such as Off
	Flag  string // the command-line flag, such as -listen
	Env   string // the environment variable
}

// settingsView is everything the settings page shows: the providers, and how
// telesfor was started.
type settingsView struct {
	Version   string
	Providers []providerView
	Streams   int // open now, over all channels
	Settings  []Setting
}

// providerView is a provider on the settings page: what to enter in Plex for
// it, its sign-in, and its channels with what is on air.
type providerView struct {
	ID       string // the provider's name in URLs
	Name     string
	Tuner    string // the address Plex adds the tuner by
	Guide    string // the XMLTV guide's address
	Channels []tuner.Station
	Account  *accountView // nil if the provider needs no account
}

// accountView is where signing in to a provider stands.
type accountView struct{ provider.Login }

func (a accountView) Pending() bool  { return a.State == provider.Pending }
func (a accountView) SignedIn() bool { return a.State == provider.SignedIn }
func (a accountView) Expired() bool  { return a.State == provider.Expired }

// Label names the state for the section's badge.
func (a accountView) Label() string {
	switch a.State {
	case provider.Pending:
		return "Waiting for the code"
	case provider.SignedIn:
		return "Signed in"
	case provider.Expired:
		return "Sign-in expired"
	}
	return "Signed out"
}

// Site is the address to enter the code at, as the page shows it.
func (a accountView) Site() string { return strings.TrimPrefix(a.URL, "https://") }

// Left is how long the code still works, in whole minutes: the page is
// refreshed every few seconds.
func (a accountView) Left() string {
	if minutes := int(time.Until(a.Expires).Minutes()) + 1; minutes > 1 {
		return strconv.Itoa(minutes) + " minutes"
	}
	return "minute"
}

func newSettingsView(providers []providerView, settings []Setting, version string) settingsView {
	v := settingsView{Version: displayVersion(version), Providers: providers, Settings: settings}
	for _, p := range providers {
		for _, ch := range p.Channels {
			v.Streams += ch.Streams
		}
	}
	return v
}

func (v settingsView) Title() string { return "Settings — telesfor" }

// Path is the URL the page refreshes from.
func (v settingsView) Path() string { return "/ui/settings" }

// State names what the tuners are doing for the header.
func (v settingsView) State() string {
	if v.Streams > 0 {
		return "on-air"
	}
	return "idle"
}

func (v settingsView) StateLabel() string {
	if v.Streams > 0 {
		return "On air"
	}
	return "Idle"
}

// providers describes each tuner's provider. The addresses are the ones the
// page was asked for at.
func (h *Handler) providers(r *http.Request) []providerView {
	views := make([]providerView, len(h.Tuners))
	for i, t := range h.Tuners {
		views[i] = providerView{
			ID:       t.Provider().Name(),
			Name:     t.Name(),
			Tuner:    t.URL(r),
			Guide:    t.URL(r) + "/xmltv.xml",
			Channels: t.Lineup(),
		}
		if account, ok := t.Provider().(provider.Account); ok {
			views[i].Account = &accountView{account.Login()}
		}
	}
	return views
}

// settings shows the page. htmx requests get only the refreshing part.
func (h *Handler) settings(w http.ResponseWriter, r *http.Request) {
	name := "layout"
	if htmx(r) {
		name = "update"
	}
	w.Header().Add("Vary", "HX-Request")
	render(w, settingsPage, name, newSettingsView(h.providers(r), h.Settings, h.Version))
}

// account finds the provider a request names, if it is one with an account.
func (h *Handler) account(r *http.Request) (provider.Account, bool) {
	for _, t := range h.Tuners {
		if account, ok := t.Provider().(provider.Account); ok && account.Name() == r.PathValue("provider") {
			return account, true
		}
	}
	return nil, false
}

// signIn starts a sign-in. The page shows how it goes, and what went wrong.
func (h *Handler) signIn(w http.ResponseWriter, r *http.Request) {
	account, ok := h.account(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if err := account.SignIn(r.Context()); err != nil {
		slog.Error("sign-in failed", "err", err)
	}
	http.Redirect(w, r, "/ui/", http.StatusSeeOther)
}

func (h *Handler) signOut(w http.ResponseWriter, r *http.Request) {
	account, ok := h.account(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if err := account.SignOut(); err != nil {
		slog.Error("sign-out failed", "err", err)
	}
	http.Redirect(w, r, "/ui/", http.StatusSeeOther)
}
