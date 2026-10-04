package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
