package httpclient

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Canceling a segment closes only its HTTP/1.1 connection. Completed segments
// can reuse a connection, and ordinary requests keep HTTP/2 with the same context.
func TestCancellableConnection(t *testing.T) {
	origin := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/blocked" {
			w.Header().Set("Content-Length", "1000000")
			w.Write([]byte("start"))
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}
		io.WriteString(w, "ok")
	}))
	origin.EnableHTTP2 = true
	origin.StartTLS()
	defer origin.Close()
	transport := NewTransport(origin.Client().Transport.(*http.Transport))
	client := &http.Client{Transport: transport}
	defer client.CloseIdleConnections()
	segments, release := SegmentClient(client)
	defer release()
	request := func(client *http.Client, ctx context.Context, path string, wantProtocol int) (*http.Response, net.Conn) {
		t.Helper()
		var conn net.Conn
		ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) { conn = info.Conn }})
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, origin.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if resp.ProtoMajor != wantProtocol {
			resp.Body.Close()
			t.Fatalf("%s used %s, want HTTP/%d", path, resp.Proto, wantProtocol)
		}
		return resp, conn
	}
	finish := func(resp *http.Response) {
		t.Helper()
		defer resp.Body.Close()
		if _, err := io.Copy(io.Discard, resp.Body); err != nil {
			t.Fatal(err)
		}
	}
	resp, ordinary := request(client, t.Context(), "/playlist", 2)
	finish(resp)
	resp, first := request(segments, t.Context(), "/segment", 1)
	finish(resp)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	resp, abandoned := request(segments, ctx, "/blocked", 1)
	if abandoned != first {
		t.Error("completed HTTP/1.1 fetch did not leave a reusable connection")
	}
	cancel()
	resp.Body.Close()
	resp, next := request(segments, t.Context(), "/segment", 1)
	finish(resp)
	if next == abandoned {
		t.Error("canceled HTTP/1.1 connection was reused")
	}
	resp, stillOrdinary := request(client, t.Context(), "/playlist", 2)
	finish(resp)
	if stillOrdinary != ordinary {
		t.Error("canceling a segment disturbed the ordinary HTTP/2 connection")
	}
}

// Two bursts of eight segment requests reuse the whole pool. Canceling a
// later request leaves another connection ready without a new dial.
func TestSegmentPoolReuse(t *testing.T) {
	const concurrent = 8
	ready := make(chan struct{}, concurrent)
	release := []chan struct{}{make(chan struct{}), make(chan struct{})}
	var connections atomic.Int32
	origin := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.ProtoMajor != 1 {
			t.Errorf("segment used %s", req.Proto)
		}
		if req.URL.Path == "/blocked" {
			w.Header().Set("Content-Length", "1000000")
			w.Write([]byte("start"))
			w.(http.Flusher).Flush()
			<-req.Context().Done()
			return
		}
		if req.URL.Path == "/first" || req.URL.Path == "/second" {
			batch := 0
			if req.URL.Path == "/second" {
				batch = 1
			}
			ready <- struct{}{}
			select {
			case <-release[batch]:
			case <-req.Context().Done():
				return
			}
		}
		io.WriteString(w, "ok")
	}))
	origin.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
	}
	origin.EnableHTTP2 = true
	origin.StartTLS()
	defer origin.Close()
	transport, closeSegments := segmentTransport(NewTransport(origin.Client().Transport.(*http.Transport)))
	defer closeSegments()
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
	defer client.CloseIdleConnections()
	for batch, path := range []string{"/first", "/second"} {
		var wg sync.WaitGroup
		for range concurrent {
			wg.Go(func() {
				resp, err := client.Get(origin.URL + path)
				if err != nil {
					t.Error(err)
					return
				}
				defer resp.Body.Close()
				if _, err := io.Copy(io.Discard, resp.Body); err != nil {
					t.Error(err)
				}
			})
		}
		for range concurrent {
			select {
			case <-ready:
			case <-time.After(5 * time.Second):
				t.Fatal("requests did not reach origin")
			}
		}
		close(release[batch])
		wg.Wait()
		if got := connections.Load(); got != concurrent {
			t.Fatalf("batch %d used %d connections, want %d reused", batch, got, concurrent)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, origin.URL+"/blocked", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	resp.Body.Close()
	resp, err = client.Get(origin.URL + "/next")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatal(err)
	}
	if got := connections.Load(); got != concurrent {
		t.Errorf("after cancellation used %d connections, want a spare reused", got)
	}
}

func TestWrap(t *testing.T) {
	origin := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Header.Get("X-Test-Auth") != "present" {
			t.Errorf("a request over %s did not go through the wrapper", req.Proto)
		}
		io.WriteString(w, "ok")
	}))
	origin.EnableHTTP2 = true
	origin.StartTLS()
	defer origin.Close()
	shared := NewTransport(origin.Client().Transport.(*http.Transport))
	defer shared.(*transport).CloseIdleConnections()
	client := &http.Client{Transport: Wrap(shared, func(req *http.Request, next http.RoundTripper) (*http.Response, error) {
		return authenticatedTransport{next}.RoundTrip(req)
	})}
	segments, release := SegmentClient(client)
	defer release()
	for client, want := range map[*http.Client]int{client: 2, segments: 1} {
		resp, err := client.Get(origin.URL)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.ProtoMajor != want {
			t.Errorf("used %s, want HTTP/%d", resp.Proto, want)
		}
	}
}

type authenticatedTransport struct{ base http.RoundTripper }

func (t authenticatedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Set("X-Test-Auth", "present")
	return t.base.RoundTrip(req)
}

func TestSegmentTransportOwnership(t *testing.T) {
	for _, kind := range []string{"plain", "default", "shared", "custom"} {
		t.Run(kind, func(t *testing.T) {
			closed := make(chan struct{}, 4)
			origin := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if kind == "custom" && req.Header.Get("X-Test-Auth") != "present" {
					t.Error("custom transport was bypassed")
				}
				io.WriteString(w, "ok")
			}))
			origin.Config.ConnState = func(_ net.Conn, state http.ConnState) {
				if state == http.StateClosed {
					closed <- struct{}{}
				}
			}
			origin.Start()
			defer origin.Close()
			base := origin.Client().Transport.(*http.Transport)
			defer base.CloseIdleConnections()
			var source http.RoundTripper = base
			switch kind {
			case "default":
				source = nil
			case "shared":
				source = NewTransport(base)
			case "custom":
				source = authenticatedTransport{base}
			}
			selected, release := segmentTransport(source)
			client := &http.Client{Transport: selected, Timeout: 3 * time.Second}
			defer client.CloseIdleConnections()
			get := func() bool {
				t.Helper()
				var reused bool
				ctx := httptrace.WithClientTrace(t.Context(), &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) { reused = info.Reused }})
				req, _ := http.NewRequestWithContext(ctx, http.MethodGet, origin.URL, nil)
				resp, err := client.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				defer resp.Body.Close()
				if _, err := io.Copy(io.Discard, resp.Body); err != nil {
					t.Fatal(err)
				}
				return reused
			}
			get()
			release()
			if kind == "plain" || kind == "default" {
				select {
				case <-closed:
				case <-time.After(time.Second):
					t.Fatal("owned pool was not closed")
				}
			} else if !get() {
				t.Error("release closed a caller-owned connection")
			}
		})
	}
}

func TestReleaseClosesConnectionAfterActiveResponse(t *testing.T) {
	finish, closed := make(chan struct{}), make(chan struct{}, 1)
	origin := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Length", "2")
		io.WriteString(w, "a")
		w.(http.Flusher).Flush()
		select {
		case <-finish:
		case <-req.Context().Done():
			return
		}
		io.WriteString(w, "b")
	}))
	origin.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateClosed {
			closed <- struct{}{}
		}
	}
	origin.Start()
	defer origin.Close()
	base := origin.Client().Transport.(*http.Transport)
	base.IdleConnTimeout = 0
	selected, release := segmentTransport(base)
	client := &http.Client{Transport: selected, Timeout: 3 * time.Second}
	defer client.CloseIdleConnections()
	resp, err := client.Get(origin.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	release() // response is still active, with one byte left at the origin
	close(finish)
	body, err := io.ReadAll(resp.Body)
	if err != nil || string(body) != "ab" {
		t.Fatalf("active response: %q, %v", body, err)
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("connection was kept idle after release")
	}
}
