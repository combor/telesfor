package provider

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/combor/telesfor/internal/httpclient"
)

type sender func(*http.Request) (*http.Response, error)

func (send sender) RoundTrip(req *http.Request) (*http.Response, error) { return send(req) }

func TestPassSend(t *testing.T) {
	tests := []struct {
		name, first, path, want string
	}{
		{"an address with the first pass", "/pass1", "/pass1/live/index.m3u8", "/pass2/live/index.m3u8"},
		{"an address without it", "/pass1", "/keys/hls.key", "/keys/hls.key"},
		{"one that only begins like it", "/pass1", "/pass10/live/index.m3u8", "/pass10/live/index.m3u8"},
		{"a stream without a pass", "", "/live/index.m3u8", "/live/index.m3u8"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var sent string
			next := sender(func(req *http.Request) (*http.Response, error) {
				sent = req.URL.EscapedPath()
				return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Request: req}, nil
			})
			req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://cdn.example"+test.path, nil)
			resp, err := NewPass(test.first, 0, nil).Send(next, req, "/pass2")
			if err != nil {
				t.Fatal(err)
			}
			if sent != test.want || resp.Request != req {
				t.Errorf("sent %s, the answer to %s: want %s, the answer to what was asked", sent, resp.Request.URL.Path, test.want)
			}
		})
	}
}

func TestPassRenew(t *testing.T) {
	asked, handing := 0, "/pass2"
	p := NewPass("/pass1", time.Hour, func(context.Context) string {
		asked++
		return handing
	})
	renew := func(refused, want, current string, wantAsked int) {
		t.Helper()
		if pass := p.Renew(t.Context(), refused); pass != want || p.Current() != current || asked != wantAsked {
			t.Errorf("%s refused: renewed to %q, asking with %s after asking %d times, want %q, %s after %d", refused, pass, p.Current(), asked, want, current, wantAsked)
		}
	}
	// A new pass that is refused is refused for something else.
	renew("/pass1", "", "/pass1", 0)
	// Once it has had its rest, a new one is asked for.
	p.signed = p.signed.Add(-time.Hour)
	renew("/pass1", "/pass2", "/pass2", 1)
	// The new one has its rest too.
	renew("/pass2", "", "/pass2", 1)
	// With no new pass to be had, the old one stays.
	p.signed = p.signed.Add(-time.Hour)
	handing = ""
	renew("/pass2", "", "/pass2", 2)

	if pass := NewPass("", 0, nil).Renew(t.Context(), ""); pass != "" {
		t.Errorf("a stream without a pass: renewed to %q, want none", pass)
	}
}

func TestPassRenewedOnce(t *testing.T) {
	asked := 0
	p := NewPass("/pass1", 0, func(context.Context) string {
		asked++
		return "/pass2"
	})
	var refused sync.WaitGroup
	for range 8 {
		refused.Go(func() {
			if pass := p.Renew(t.Context(), "/pass1"); pass != "/pass2" {
				t.Errorf("renewed to %q, want /pass2", pass)
			}
		})
	}
	refused.Wait()
	if asked != 1 {
		t.Errorf("a new pass was asked for %d times, want once", asked)
	}
}

// A refused segment is sent again over the segment pool with its range; the
// new pass and the playlists after go over the ordinary transport.
func TestPassOverTheSegmentPool(t *testing.T) {
	var mu sync.Mutex
	seen := map[string][]int{}
	origin := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		mu.Lock()
		seen[req.URL.Path] = append(seen[req.URL.Path], req.ProtoMajor)
		mu.Unlock()
		switch req.URL.Path {
		case "/old/segment.ts":
			w.WriteHeader(http.StatusForbidden)
		case "/new/segment.ts":
			if req.Header.Get("Range") != "bytes=2-5" {
				t.Error("the renewed segment lost its byte range")
			}
			io.WriteString(w, "part")
		case "/new/media.m3u8":
			io.WriteString(w, "#EXTM3U\nsegment.ts\n")
		case "/sign":
			io.WriteString(w, "/new")
		default:
			http.NotFound(w, req)
		}
	}))
	origin.EnableHTTP2 = true
	origin.StartTLS()
	defer origin.Close()
	base := &http.Client{Transport: httpclient.NewTransport(origin.Client().Transport.(*http.Transport))}
	defer base.CloseIdleConnections()
	p := NewPass("/old", 0, func(ctx context.Context) string {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, origin.URL+"/sign", nil)
		resp, err := base.Do(req)
		if err != nil {
			t.Error(err)
			return ""
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return string(body)
	})
	client := &http.Client{Transport: httpclient.Wrap(base.Transport, func(req *http.Request, next http.RoundTripper) (*http.Response, error) {
		pass := p.Current()
		resp, err := p.Send(next, req, pass)
		if err == nil && resp.StatusCode == http.StatusForbidden {
			if pass = p.Renew(req.Context(), pass); pass != "" {
				resp.Body.Close()
				return p.Send(next, req, pass)
			}
		}
		return resp, err
	})}
	segments, release := httpclient.SegmentClient(client)
	defer release()
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, origin.URL+"/old/segment.ts", nil)
	req.Header.Set("Range", "bytes=2-5")
	resp, err := segments.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || string(body) != "part" {
		t.Fatalf("renewed segment: %q, %v", body, err)
	}
	if resp, err = client.Get(origin.URL + "/old/media.m3u8"); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	mu.Lock()
	defer mu.Unlock()
	for path, want := range map[string]int{"/old/segment.ts": 1, "/new/segment.ts": 1, "/sign": 2, "/new/media.m3u8": 2} {
		if !slices.Equal(seen[path], []int{want}) {
			t.Errorf("%s used %v, want HTTP/%d once", path, seen[path], want)
		}
	}
}
