package main

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/combor/telesfor/internal/web"
)

func TestHealthURL(t *testing.T) {
	for listen, want := range map[string]string{
		":5004":           "http://127.0.0.1:5004/lineup_status.json",
		"0.0.0.0:5004":    "http://127.0.0.1:5004/lineup_status.json",
		"[::]:5004":       "http://[::1]:5004/lineup_status.json",
		"[fd00::5]:5004":  "http://[fd00::5]:5004/lineup_status.json",
		"192.0.2.10:5004": "http://192.0.2.10:5004/lineup_status.json",
		"telesfor:5004":   "http://telesfor:5004/lineup_status.json",
	} {
		if got, err := healthURL(listen); err != nil || got != want {
			t.Errorf("healthURL(%q) = %q, %v; want %q", listen, got, err, want)
		}
	}
	if _, err := healthURL("5004"); err == nil {
		t.Error("healthURL accepted a listen address without a port")
	}
}

func TestCheckHealth(t *testing.T) {
	status := http.StatusOK
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/lineup_status.json" {
			t.Errorf("unexpected request: %s", r.URL)
		}
		w.WriteHeader(status)
	}))
	listen := strings.TrimPrefix(server.URL, "http://")

	if err := checkHealth(listen); err != nil {
		t.Errorf("checkHealth of a telesfor on the air: %v", err)
	}
	status = http.StatusServiceUnavailable
	if err := checkHealth(listen); err == nil {
		t.Error("checkHealth passed a telesfor that answers 503")
	}
	server.Close()
	if err := checkHealth(listen); err == nil {
		t.Error("checkHealth passed with nothing listening")
	}
}

func TestSettings(t *testing.T) {
	got := settings(":5004", "http://user:secret@proxy.example:8888", "", "/var/lib/telesfor", true)
	want := []web.Setting{
		{Name: "Listen address", Value: ":5004", Flag: "-listen", Env: "TELESFOR_LISTEN"},
		// Without the login: the settings page is open to the network.
		{Name: "TVP proxy", Value: "http://proxy.example:8888", Flag: "-tvp-proxy", Env: "TELESFOR_TVP_PROXY"},
		{Name: "Globoplay proxy", State: "Not set", Flag: "-globo-proxy", Env: "TELESFOR_GLOBO_PROXY"},
		{Name: "Data directory", Value: "/var/lib/telesfor", Flag: "-data", Env: "TELESFOR_DATA"},
		{Name: "Debug logging", State: "On", Flag: "-debug", Env: "TELESFOR_DEBUG"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("settings = %+v, want %+v", got, want)
	}
	if got := settings(":5004", "", "", "", false); got[1].State != "Not set" || got[4].State != "Off" {
		t.Errorf("settings without a proxy or debug logging = %+v", got)
	}
}
