package web

import (
	"context"
	"log/slog"
	"net"
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

const serverPath = "/ui/server"

// tabPath is where a provider's tab is.
func tabPath(p provider.Provider) string { return "/ui/providers/" + p.Name() }

// frame is what every tab has around its own part: the header, and the tabs.
type frame struct {
	Version string
	Tabs    []tab
	Title   string
	Path    string // the URL the page refreshes from
	OnAir   bool
}

// tab leads to a provider's part of the page, or to the server's.
type tab struct {
	Name     string
	Path     string
	Current  bool
	Disabled bool   // the tab of a provider that is
	Problem  string // what the provider needs seen to, such as a sign-in to renew
}

// State names what the tuners are doing for the header.
func (f frame) State() string {
	if f.OnAir {
		return "on-air"
	}
	return "idle"
}

func (f frame) StateLabel() string {
	if f.OnAir {
		return "On air"
	}
	return "Idle"
}

// frame lists the tabs, with the one at path as the current.
func (h *Handler) frame(path string) frame {
	f := frame{Version: displayVersion(h.Version), Path: path}
	for _, p := range h.Providers {
		t := tab{Name: p.Tuner.Name(), Path: tabPath(p.Tuner.Provider()), Disabled: !p.Tuner.Enabled()}
		for _, ch := range p.Tuner.Lineup() {
			f.OnAir = f.OnAir || ch.Streams > 0
		}
		if account, ok := p.Tuner.Provider().(provider.Account); ok && !t.Disabled {
			if login := (accountView{account.Login()}); login.Expired() {
				t.Problem = login.Label()
			}
		}
		f.Tabs = append(f.Tabs, t)
	}
	f.Tabs = append(f.Tabs, tab{Name: "Server", Path: serverPath})
	for i, t := range f.Tabs {
		if t.Path == path {
			f.Tabs[i].Current, f.Title = true, t.Name+" — telesfor"
		}
	}
	return f
}

// providerView is a provider's tab: what to enter in Plex for it, its sign-in,
// its settings, the switch that disables it, and its channels with what is on
// air. The tab of a disabled provider has the switch alone.
type providerView struct {
	frame
	Name     string
	Disabled bool
	Failed   bool         // it could not be enabled or disabled when last asked
	Tuner    string       // the address Plex adds the tuner by
	Guide    string       // the XMLTV guide's address
	Account  *accountView // nil if the provider needs no account
	Settings []Setting    // of the provider, as telesfor was started with
	Channels []tuner.Station
}

// serverView is the server's tab: how telesfor was started.
type serverView struct {
	frame
	Settings []Setting
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

// Site is the address to enter the code at, as the page shows it: without
// what a provider adds to have the code filled in.
func (a accountView) Site() string {
	site, _, _ := strings.Cut(strings.TrimPrefix(a.URL, "https://"), "?")
	return site
}

// Left is how long the code still works, in whole minutes: the page is
// refreshed every few seconds.
func (a accountView) Left() string {
	if minutes := int(time.Until(a.Expires).Minutes()) + 1; minutes > 1 {
		return strconv.Itoa(minutes) + " minutes"
	}
	return "minute"
}

// find looks up the provider a request names. The page's own address is the
// tab of the first provider that is enabled, or of the first if none is.
func (h *Handler) find(r *http.Request) (Provider, bool) {
	name := r.PathValue("provider")
	for _, p := range h.Providers {
		if p.Tuner.Provider().Name() == name || name == "" && p.Tuner.Enabled() {
			return p, true
		}
	}
	if name == "" && len(h.Providers) > 0 {
		return h.Providers[0], true
	}
	return Provider{}, false
}

// providerTab shows a provider's tab.
func (h *Handler) providerTab(w http.ResponseWriter, r *http.Request) {
	p, ok := h.find(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	t := p.Tuner
	address := t.Address(reached(r))
	view := providerView{
		frame:    h.frame(tabPath(t.Provider())),
		Name:     t.Name(),
		Disabled: !t.Enabled(),
		Tuner:    address,
		Guide:    address + "/xmltv.xml",
		Settings: p.Settings,
		Channels: t.Lineup(),
	}
	_, view.Failed = h.failed.Load(t.Provider().Name())
	if account, ok := t.Provider().(provider.Account); ok {
		view.Account = &accountView{account.Login()}
	}
	render(w, r, providerPage, view)
}

// reached is the host the page reached telesfor at, for the addresses to enter
// in Plex. A reverse proxy asks by a name of its own, which is not where Plex
// finds the tuners: the address the proxy connected to is.
func reached(r *http.Request) string {
	local, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
	for _, proxied := range []string{"X-Forwarded-For", "X-Forwarded-Host", "Forwarded"} {
		if ok && r.Header.Get(proxied) != "" {
			return local.String()
		}
	}
	return r.Host
}

func (h *Handler) serverTab(w http.ResponseWriter, r *http.Request) {
	render(w, r, serverPage, serverView{h.frame(serverPath), h.Settings})
}

// account finds the provider a request names, if it is one with an account.
func (h *Handler) account(r *http.Request) (provider.Account, bool) {
	if p, ok := h.find(r); ok {
		account, ok := p.Tuner.Provider().(provider.Account)
		return account, ok
	}
	return nil, false
}

// signIn starts a sign-in. The tab shows how it goes, and what went wrong.
func (h *Handler) signIn(w http.ResponseWriter, r *http.Request) {
	account, ok := h.account(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if err := account.SignIn(r.Context()); err != nil {
		slog.Error("sign-in failed", "provider", account.Name(), "err", err)
	}
	http.Redirect(w, r, tabPath(account), http.StatusSeeOther)
}

func (h *Handler) signOut(w http.ResponseWriter, r *http.Request) {
	account, ok := h.account(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if err := account.SignOut(); err != nil {
		slog.Error("sign-out failed", "provider", account.Name(), "err", err)
	}
	http.Redirect(w, r, tabPath(account), http.StatusSeeOther)
}

// switchTo enables or disables a provider from its tab, which shows whether
// that failed.
func (h *Handler) switchTo(enabled bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := h.find(r)
		if !ok {
			http.NotFound(w, r)
			return
		}
		h.setEnabled(r.Context(), p, enabled)
		http.Redirect(w, r, tabPath(p.Tuner.Provider()), http.StatusSeeOther)
	})
}

// setEnabled enables or disables a provider, and notes for its tab whether
// that failed.
func (h *Handler) setEnabled(ctx context.Context, p Provider, enabled bool) error {
	var err error
	if enabled {
		err = p.Tuner.Enable(ctx)
	} else {
		err = p.Tuner.Disable()
	}
	name := p.Tuner.Provider().Name()
	if err != nil {
		slog.Error("enabling or disabling failed", "provider", name, "enable", enabled, "err", err)
		h.failed.Store(name, true)
		return err
	}
	h.failed.Delete(name)
	return nil
}
