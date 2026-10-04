package web

import (
	"net/http"

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

// settingsView is everything the settings page shows: what to enter in Plex,
// the lineup with what is on air, and how telesfor was started.
type settingsView struct {
	Version  string
	Tuner    string // the address Plex adds the tuner by
	Guide    string // the XMLTV guide's address
	Channels []tuner.Station
	Streams  int // open now, over all channels
	Settings []Setting
}

// newSettingsView takes base as tuner.BaseURL returns it.
func newSettingsView(base string, lineup []tuner.Station, settings []Setting, version string) settingsView {
	v := settingsView{
		Version:  displayVersion(version),
		Tuner:    base,
		Guide:    base + "/xmltv.xml",
		Channels: lineup,
		Settings: settings,
	}
	for _, ch := range lineup {
		v.Streams += ch.Streams
	}
	return v
}

func (v settingsView) Title() string { return "Settings — telesfor" }

// Path is the URL the page refreshes from.
func (v settingsView) Path() string { return "/ui/settings" }

// State names what the tuner is doing for the header.
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

// settings shows the page. htmx requests get only the refreshing part.
func (h *Handler) settings(w http.ResponseWriter, r *http.Request) {
	name := "layout"
	if htmx(r) {
		name = "update"
	}
	w.Header().Add("Vary", "HX-Request")
	render(w, settingsPage, name, newSettingsView(tuner.BaseURL(r), h.Tuner.Lineup(), h.Settings, h.Version))
}
