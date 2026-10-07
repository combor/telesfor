package remux

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/combor/telesfor/internal/httpclient"
)

func TestRelayClosesOwnedSegmentPool(t *testing.T) {
	closed := make(chan struct{}, 1)
	origin := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) { io.WriteString(w, "ok") }))
	origin.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateClosed {
			closed <- struct{}{}
		}
	}
	origin.EnableHTTP2 = true
	origin.StartTLS()
	defer origin.Close()
	relay, _, err := openRelay(origin.URL+"/playlist", origin.Client())
	if err != nil {
		t.Fatal(err)
	}
	defer relay.segments.CloseIdleConnections()
	resp, err := relay.segments.Get(origin.URL + "/asset")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	relay.close()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("relay left its private segment connection open")
	}
}

// Read audio through the stage rather than seeding the flow counter. This
// must fail if audio is disconnected from the shared speed measurement.
func TestAudioContributesToFlow(t *testing.T) {
	const chunk = 32 << 10
	rest := make(chan struct{})
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Length", "65536")
		w.Write(make([]byte, chunk))
		w.(http.Flusher).Flush()
		select {
		case <-rest:
		case <-req.Context().Done():
			return
		}
		w.Write(make([]byte, chunk))
	}))
	defer origin.Close()
	relay, _, err := openRelay(origin.URL+"/playlist", origin.Client())
	if err != nil {
		t.Fatal(err)
	}
	defer relay.close()
	defer relay.segments.CloseIdleConnections()
	now := time.Now()
	ctl := testController(&route{streams: map[*controller]float64{}}, &now)
	st, err := relay.perform(tvpLadder, ctl, newMeter(io.Discard), func(*leg) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	defer st.close()
	l, err := st.begin(0)
	if err != nil {
		t.Fatal(err)
	}
	address, _ := url.Parse(origin.URL + "/audio")
	l.sound.list = playlist{segments: []entry{{seq: 0, uri: address, length: time.Second}}}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+st.server.Addr+"/1/a/0/asset", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if _, err := io.CopyN(io.Discard, resp.Body, chunk); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * atOnce) // put the remaining bytes beyond the initial read burst
	close(rest)
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatal(err)
	}
	st.flow.mu.Lock()
	defer st.flow.mu.Unlock()
	var counted int64
	for _, n := range st.flow.tenths {
		counted += n
	}
	if counted < chunk {
		t.Fatalf("counted %d audio bytes, want at least %d", counted, chunk)
	}
}

func TestUnwatchedSegmentCancellation(t *testing.T) {
	for _, test := range []struct {
		name, track string
		cut         bool
	}{
		{"audio/viewer-leaves", "a", false},
		{"audio/leg-cut-before-headers", "a", true},
		{"lowest-video/viewer-leaves", "v", false},
		{"lowest-video/leg-cut-before-headers", "v", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			asked, release, canceled := make(chan int, 1), make(chan struct{}), make(chan struct{})
			origin := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if req.URL.Path == "/next" {
					io.WriteString(w, "ok")
					return
				}
				asked <- req.ProtoMajor
				select {
				case <-release:
				case <-req.Context().Done():
					return
				}
				w.Header().Set("Content-Length", "1000000")
				w.Write(make([]byte, 32<<10))
				w.(http.Flusher).Flush()
				<-req.Context().Done()
				close(canceled)
			}))
			origin.EnableHTTP2 = true
			origin.StartTLS()
			defer origin.Close()
			client := &http.Client{Transport: httpclient.NewTransport(origin.Client().Transport.(*http.Transport))}
			defer client.CloseIdleConnections()
			relay, _, err := openRelay(origin.URL+"/playlist", client)
			if err != nil {
				t.Fatal(err)
			}
			defer relay.close()
			now := time.Now()
			ctl := testController(&route{streams: map[*controller]float64{}}, &now)
			st, err := relay.perform(tvpLadder, ctl, newMeter(io.Discard), func(*leg) error { return nil })
			if err != nil {
				t.Fatal(err)
			}
			defer st.close()
			l, err := st.begin(0)
			if err != nil {
				t.Fatal(err)
			}
			address, _ := url.Parse(origin.URL + "/asset")
			list := playlist{segments: []entry{{seq: 0, uri: address, length: time.Second}}}
			l.video.list, l.sound.list = list, list

			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+st.server.Addr+"/1/"+test.track+"/0/asset", nil)
			var resp *http.Response
			var fetchErr error
			answered := make(chan struct{})
			go func() {
				resp, fetchErr = http.DefaultClient.Do(req)
				close(answered)
			}()
			select {
			case protocol := <-asked:
				if protocol != 1 {
					t.Fatalf("unwatched segment used HTTP/%d, want HTTP/1", protocol)
				}
			case <-ctx.Done():
				t.Fatal("origin was not reached")
			}
			if test.cut {
				st.mu.Lock()
				l.cut()
				st.mu.Unlock()
			}
			close(release)
			<-answered // the request's deadline also bounds this wait
			if fetchErr != nil {
				t.Fatal(fetchErr)
			}
			defer resp.Body.Close()
			if test.cut {
				if resp.StatusCode != http.StatusNotFound {
					t.Fatalf("cut segment returned %d", resp.StatusCode)
				}
			} else {
				if _, err := io.CopyN(io.Discard, resp.Body, 1); err != nil {
					t.Fatal(err)
				}
				cancel()
			}
			resp.Body.Close()
			select {
			case <-canceled:
			case <-time.After(3 * time.Second):
				t.Fatal("upstream body was not canceled")
			}
			resp, err = client.Get(origin.URL + "/next")
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.ProtoMajor != 2 {
				t.Errorf("ordinary request used %s", resp.Proto)
			}
			if _, err := io.Copy(io.Discard, resp.Body); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// The non-adaptive relay also closes canceled transfers, including URLs
// whose names say nothing about the media they contain.
func TestRelayCancelsOpaqueMedia(t *testing.T) {
	canceled := make(chan struct{})
	origin := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/api" {
			if req.ProtoMajor != 2 {
				t.Errorf("API used %s", req.Proto)
			}
			io.WriteString(w, "ok")
			return
		}
		if req.ProtoMajor != 1 {
			t.Errorf("relayed media used %s", req.Proto)
		}
		if req.Header.Get("Range") != "bytes=0-" {
			t.Error("media request lost its range")
		}
		w.Header().Set("Content-Length", "1000000")
		w.Write(make([]byte, 32<<10))
		w.(http.Flusher).Flush()
		<-req.Context().Done()
		close(canceled)
	}))
	origin.EnableHTTP2 = true
	origin.StartTLS()
	defer origin.Close()
	client := &http.Client{Transport: httpclient.NewTransport(origin.Client().Transport.(*http.Transport))}
	defer client.CloseIdleConnections()
	relay, local, err := openRelay(origin.URL+"/opaque?part=1", client)
	if err != nil {
		t.Fatal(err)
	}
	defer relay.close()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, local, nil)
	req.Header.Set("Range", "bytes=0-")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if _, err := io.CopyN(io.Discard, resp.Body, 1); err != nil {
		t.Fatal(err)
	}
	cancel()
	resp.Body.Close()
	select {
	case <-canceled:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream media was not canceled")
	}
	resp, err = client.Get(origin.URL + "/api")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatal(err)
	}
}
