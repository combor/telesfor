package remux

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/combor/telesfor/internal/httpclient"
)

// response is what a fetch through the relay came back with.
type response struct {
	status int
	url    *url.URL // where it came from in the end, after redirects
	header http.Header
	body   string
}

// get fetches a URI the way ffmpeg does: resolved against the URL of the
// manifest it was found in, and following redirects.
func get(t *testing.T, base *url.URL, ref string) response {
	t.Helper()
	target, err := base.Parse(ref)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Get(target.String())
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return response{resp.StatusCode, resp.Request.URL, resp.Header, string(body)}
}

// relayTo returns a relay for manifest and the local address ffmpeg would
// read it at.
func relayTo(t *testing.T, manifest string, client *http.Client) (*relay, *url.URL) {
	t.Helper()
	r, local, err := openRelay(manifest, client)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.close)
	return r, mustParse(t, local)
}

func TestRelay(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.RequestURI() {
		case "/live/token/master.m3u8":
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			io.WriteString(w, "playlist")
		case "/live/token/video/segment.mp4?part=1":
			io.WriteString(w, "below")
		case "/live/audio/segment.mp4":
			io.WriteString(w, "beside")
		case "/keys/key.bin":
			io.WriteString(w, "elsewhere")
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(upstream.Close)

	relay, manifest := relayTo(t, upstream.URL+"/live/token/master.m3u8", upstream.Client())

	if got := get(t, manifest, ""); got.status != 200 || got.header.Get("Content-Type") != "application/vnd.apple.mpegurl" || got.body != "playlist" {
		t.Errorf("manifest: got %d %q %q, want it relayed as is", got.status, got.header.Get("Content-Type"), got.body)
	}
	// Whatever relative URI a manifest contains must lead to the same file as
	// it does on the upstream server.
	for ref, want := range map[string]string{
		"video/segment.mp4?part=1": "below",
		"../audio/segment.mp4":     "beside",
		"/keys/key.bin":            "elsewhere",
	} {
		if got := get(t, manifest, ref); got.status != 200 || got.body != want {
			t.Errorf("%s: got %d %q, want %q", ref, got.status, got.body, want)
		}
	}
	if got := get(t, manifest, "missing.mp4"); got.status != 404 {
		t.Errorf("missing file: got %d, want the upstream's 404", got.status)
	}

	relay.close()
	if resp, err := http.Get(manifest.String()); err == nil {
		resp.Body.Close()
		t.Error("the relay still answers after it was closed")
	}
}

// A playlist may address its segments as byte ranges of one file, so a request
// for part of a file must return that part.
func TestRelayByteRange(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "media.mp4", time.Time{}, strings.NewReader("0123456789"))
	}))
	t.Cleanup(upstream.Close)

	_, local := relayTo(t, upstream.URL+"/media.mp4", upstream.Client())

	req, _ := http.NewRequest(http.MethodGet, local.String(), nil)
	req.Header.Set("Range", "bytes=2-5")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusPartialContent || resp.Header.Get("Content-Range") != "bytes 2-5/10" || string(body) != "2345" {
		t.Errorf("got %d %q %q, want bytes 2 to 5", resp.StatusCode, resp.Header.Get("Content-Range"), body)
	}
}

// An upstream transfer that breaks off must not reach ffmpeg looking complete.
func TestRelayInterruptedTransfer(t *testing.T) {
	// A file that is passed on as it arrives, and one the size of an MPEG-TS
	// segment, which is passed on in whole packets.
	for _, size := range []int{10, 2 * packetSize} {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Length", strconv.Itoa(size))
			w.Write(make([]byte, size/2)) // half of it, and then the connection drops
		}))
		t.Cleanup(upstream.Close)

		_, local := relayTo(t, upstream.URL+"/segment", upstream.Client())

		resp, err := http.Get(local.String())
		if err != nil {
			continue // failed before the first byte: just as good
		}
		defer resp.Body.Close()
		if body, err := io.ReadAll(resp.Body); err == nil {
			t.Errorf("read %d of %d bytes without an error, want the transfer to fail as it did upstream", len(body), size)
		}
	}
}

// An MPEG-TS segment whose frames are stamped to be decoded after they are
// shown must reach ffmpeg with its timestamps repaired.
func TestRelayRepairsTimestamps(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		file := tvpSegment(900000, frameLength)
		if r.URL.Path == "/init.mp4" {
			file = file[1:] // the same bytes, but not as whole packets
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(file)))
		// A file arrives in pieces that end anywhere, not where packets end.
		for piece := range slices.Chunk(file, 100) {
			w.Write(piece)
			w.(http.Flusher).Flush()
			time.Sleep(time.Millisecond)
		}
	}))
	t.Cleanup(upstream.Close)

	_, manifest := relayTo(t, upstream.URL+"/playlist.m3u8", upstream.Client())

	// The first segment tells the relay how late the stream is. From then on
	// every frame is repaired.
	get(t, manifest, "segment1.ts")
	if got := get(t, manifest, "segment2.ts"); got.status != 200 || got.body != string(tvpSegment(900000, 0)) {
		t.Errorf("segment: got %d and %d bytes, want the segment with its frames decoded in time", got.status, len(got.body))
	}
	if got := get(t, manifest, "init.mp4"); got.status != 200 || got.body != string(tvpSegment(900000, frameLength)[1:]) {
		t.Errorf("another file: got %d and %d bytes, want it relayed as is", got.status, len(got.body))
	}
}

// A CDN may hand out the URL of a router that redirects to the server holding
// the stream, and serves no media itself. The player must see that redirect,
// pointing at the relay rather than at the server.
func TestRelayRedirect(t *testing.T) {
	edge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/stream/index.m3u8":
			io.WriteString(w, "playlist")
		case "/stream/segment.mp4":
			io.WriteString(w, "segment")
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(edge.Close)
	router := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/live.m3u8" {
			http.Error(w, "the router serves no media", http.StatusPreconditionFailed)
			return
		}
		http.Redirect(w, r, edge.URL+"/stream/index.m3u8", http.StatusFound)
	}))
	t.Cleanup(router.Close)

	_, start := relayTo(t, router.URL+"/live.m3u8", router.Client())

	manifest := get(t, start, "")
	if manifest.status != 200 || manifest.body != "playlist" {
		t.Fatalf("manifest: got %d %q, want the redirect followed", manifest.status, manifest.body)
	}
	if host := manifest.url.Host; host == start.Host || "http://"+host == edge.URL {
		t.Fatalf("manifest came from %s, want a second twin standing in for the edge", host)
	}
	if got := get(t, manifest.url, "segment.mp4"); got.status != 200 || got.body != "segment" {
		t.Errorf("relative URI: got %d %q, want the file next to where the manifest ended up", got.status, got.body)
	}
	// A live playlist is fetched over and over from the URL it was first found at.
	if again := get(t, start, ""); again.status != 200 || again.body != "playlist" {
		t.Errorf("manifest, again: got %d %q, want it redirected as before", again.status, again.body)
	}
}

// A manifest is fetched to see whether it lists qualities to choose from. One
// that does not is kept for ffmpeg's first look at it, and fetched anew for
// every look after: a live playlist changes.
func TestRelayKeepsManifest(t *testing.T) {
	const playlist = "#EXTM3U\n#EXTINF:2.000,\nsegment.ts\n"
	var fetched atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetched.Add(1)
		io.WriteString(w, playlist)
	}))
	t.Cleanup(upstream.Close)

	relay, manifest := relayTo(t, upstream.URL+"/media.m3u8", upstream.Client())
	if l := relay.qualities(t.Context(), upstream.URL+"/media.m3u8"); l != nil {
		t.Fatalf("found %d qualities in the playlist of one", len(l.qualities))
	}
	for look, want := range []int32{1, 2} {
		if got := get(t, manifest, ""); got.status != 200 || got.body != playlist || fetched.Load() != want {
			t.Errorf("look %d: got %d %q after %d fetches, want the playlist after %d", look+1, got.status, got.body, fetched.Load(), want)
		}
	}
	if !relay.short.Load() {
		t.Error("the kept playlist of one segment was not noted as short")
	}
}

// The relay notes a playlist that holds fewer segments than the head start
// asks for. A master playlist, which lists none, says nothing either way.
func TestRelayNotesShortPlaylist(t *testing.T) {
	segments := 3
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/master.m3u8" {
			io.WriteString(w, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=716800\nmedia.m3u8\n")
			return
		}
		io.WriteString(w, "#EXTM3U\n"+strings.Repeat("#EXTINF:2.000,\nsegment.ts\n", segments))
	}))
	t.Cleanup(upstream.Close)

	relay, manifest := relayTo(t, upstream.URL+"/master.m3u8", upstream.Client())

	for _, want := range []bool{true, false} {
		get(t, manifest, "media.m3u8")
		get(t, manifest, "")
		if got := relay.short.Load(); got != want {
			t.Errorf("a playlist of %d segments: short = %v, want %v", segments, got, want)
		}
		segments = headStart
	}
}

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
	t.Cleanup(origin.Close)
	relay, _ := relayTo(t, origin.URL+"/playlist", origin.Client())
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
	t.Cleanup(origin.Close)
	client := &http.Client{Transport: httpclient.NewTransport(origin.Client().Transport.(*http.Transport))}
	t.Cleanup(client.CloseIdleConnections)
	_, local := relayTo(t, origin.URL+"/opaque?part=1", client)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, local.String(), nil)
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
