package cultura

import (
	"context"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

type sender func(*http.Request) (*http.Response, error)

func (send sender) RoundTrip(req *http.Request) (*http.Response, error) { return send(req) }

type responseBody struct {
	io.Reader
	closed bool
}

func (b *responseBody) Close() error {
	b.closed = true
	return nil
}

func TestGetRetries(t *testing.T) {
	type testCase struct {
		name     string
		statuses []int
		at       []time.Duration
	}
	tests := []testCase{
		{"recovers on the last try", []int{503, 502, 504, 200}, []time.Duration{0, time.Second, 3 * time.Second, 7 * time.Second}},
		{"internal server error", []int{500, 200}, []time.Duration{0, time.Second}},
		{"stays down", []int{503, 503, 503, 502}, []time.Duration{0, time.Second, 3 * time.Second, 7 * time.Second}},
	}
	for _, status := range []int{200, 400, 401, 403, 404, 429, 451, 501} {
		tests = append(tests, testCase{http.StatusText(status), []int{status}, []time.Duration{0}})
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				started := time.Now()
				var asked []time.Duration
				var body *responseBody
				p := &Provider{client: &http.Client{Transport: sender(func(req *http.Request) (*http.Response, error) {
					if body != nil && !body.closed {
						t.Fatal("previous response is still open")
					}
					if len(asked) >= len(test.statuses) {
						t.Fatal("made an extra request")
					}
					status := test.statuses[len(asked)]
					asked = append(asked, time.Since(started))
					body = &responseBody{Reader: strings.NewReader(http.StatusText(status))}
					return &http.Response{StatusCode: status, Body: body, Request: req}, nil
				})}}
				const address = "https://cultura.example/grade/10102026.html"
				page, at, status, err := p.get(t.Context(), address)
				want := test.statuses[len(test.statuses)-1]
				if err != nil || status != want || page != http.StatusText(want) || at == nil || at.String() != address {
					t.Errorf("get() = %q, %v, %d, %v: want the final response with status %d", page, at, status, err, want)
				}
				if !slices.Equal(asked, test.at) || time.Since(started) != test.at[len(test.at)-1] {
					t.Errorf("requests at %v, returned after %s: want %v with no further wait", asked, time.Since(started), test.at)
				}
				if body == nil || !body.closed {
					t.Error("final response was not closed")
				}
			})
		})
	}
}

func TestGetCanceled(t *testing.T) {
	for _, after := range []time.Duration{0, 500 * time.Millisecond, 1500 * time.Millisecond} {
		t.Run(after.String(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				if after == 0 {
					cancel()
				} else {
					go func() {
						time.Sleep(after)
						cancel()
					}()
				}
				asked := 0
				p := &Provider{client: &http.Client{Transport: sender(func(req *http.Request) (*http.Response, error) {
					asked++
					return &http.Response{StatusCode: http.StatusServiceUnavailable, Body: http.NoBody, Request: req}, nil
				})}}
				started := time.Now()
				_, _, _, err := p.get(ctx, "https://cultura.example/grade/10102026.html")
				wantAsked := 0
				if after > 0 {
					wantAsked = 1
				}
				if after > time.Second {
					wantAsked = 2
				}
				if !errors.Is(err, context.Canceled) || time.Since(started) != after || asked != wantAsked {
					t.Errorf("get() = %v after %s and %d requests: want cancellation after %s and %d requests", err, time.Since(started), asked, after, wantAsked)
				}
			})
		})
	}
}

func TestGetDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
		defer cancel()
		asked := 0
		p := &Provider{client: &http.Client{Transport: sender(func(req *http.Request) (*http.Response, error) {
			asked++
			return &http.Response{StatusCode: http.StatusServiceUnavailable, Body: http.NoBody, Request: req}, nil
		})}}
		started := time.Now()
		_, _, _, err := p.get(ctx, "https://cultura.example/grade/10102026.html")
		if !errors.Is(err, context.DeadlineExceeded) || asked != 1 || time.Since(started) != 500*time.Millisecond {
			t.Errorf("get() = %v after %s and %d requests: want the deadline to stop the first wait", err, time.Since(started), asked)
		}
	})
}

func TestGetTransportError(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		failed := errors.New("connection failed")
		asked := 0
		p := &Provider{client: &http.Client{Transport: sender(func(*http.Request) (*http.Response, error) {
			asked++
			return nil, failed
		})}}
		started := time.Now()
		_, _, _, err := p.get(t.Context(), "https://cultura.example/grade/10102026.html")
		if !errors.Is(err, failed) || asked != 1 || time.Since(started) != 0 {
			t.Errorf("get() = %v after %s and %d requests: want the transport error without retrying", err, time.Since(started), asked)
		}
	})
}

func TestProgrammesRetries(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		asked := make(map[string]int)
		p := &Provider{
			channels: []channel{{id: "tv-cultura", name: "TV Cultura", guide: "https://cultura.example/grade"}},
			client: &http.Client{Transport: sender(func(req *http.Request) (*http.Response, error) {
				asked[req.URL.Path]++
				if asked[req.URL.Path] == 1 {
					return &http.Response{StatusCode: http.StatusServiceUnavailable, Body: http.NoBody, Request: req}, nil
				}
				page := entry("06:00", "Roda Viva", "", "", "") + entry("07:00", "Balaio", "", "", "")
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(page)), Request: req}, nil
			})},
		}
		channels, _ := p.Channels(t.Context())
		from := time.Date(2026, 10, 10, 6, 0, 0, 0, brt)
		started := time.Now()
		programmes, err := p.Programmes(t.Context(), channels, from, from.Add(time.Hour))
		if err != nil || len(programmes) != 1 || programmes[0].Title != "Roda Viva" {
			t.Errorf("Programmes() = %v, %v: want the published programme after retrying", programmes, err)
		}
		if len(asked) != 2 || asked["/grade/09102026.html"] != 2 || asked["/grade/10102026.html"] != 2 || time.Since(started) != 2*time.Second {
			t.Errorf("asked for %v over %s: want both days tried twice, one second apart each", asked, time.Since(started))
		}
	})
}
