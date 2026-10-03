package remux

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
	defer upstream.Close()

	relay, local, err := openRelay(upstream.URL+"/live/token/master.m3u8", upstream.Client())
	if err != nil {
		t.Fatal(err)
	}
	manifest, _ := url.Parse(local)

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
	if resp, err := http.Get(local); err == nil {
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
	defer upstream.Close()

	relay, local, err := openRelay(upstream.URL+"/media.mp4", upstream.Client())
	if err != nil {
		t.Fatal(err)
	}
	defer relay.close()

	req, _ := http.NewRequest(http.MethodGet, local, nil)
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
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "10")
		io.WriteString(w, "01234") // half of it, and then the connection drops
	}))
	defer upstream.Close()

	relay, local, err := openRelay(upstream.URL+"/segment.mp4", upstream.Client())
	if err != nil {
		t.Fatal(err)
	}
	defer relay.close()

	resp, err := http.Get(local)
	if err != nil {
		return // failed before the first byte: just as good
	}
	defer resp.Body.Close()
	if body, err := io.ReadAll(resp.Body); err == nil {
		t.Errorf("read %q without an error, want the transfer to fail as it did upstream", body)
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
	defer edge.Close()
	router := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/live.m3u8" {
			http.Error(w, "the router serves no media", http.StatusPreconditionFailed)
			return
		}
		http.Redirect(w, r, edge.URL+"/stream/index.m3u8", http.StatusFound)
	}))
	defer router.Close()

	relay, local, err := openRelay(router.URL+"/live.m3u8", router.Client())
	if err != nil {
		t.Fatal(err)
	}
	defer relay.close()
	start, _ := url.Parse(local)

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

// TestCopy remuxes a short generated HLS stream end to end with ffmpeg, through
// a relay and across a redirect.
func TestCopy(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg is not installed")
	}

	dir := t.TempDir()
	generate := exec.Command(ffmpeg, "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc=duration=2:size=160x120:rate=25",
		"-f", "lavfi", "-i", "sine=duration=2",
		"-c:v", "mpeg2video", "-c:a", "mp2",
		"-f", "hls", "-hls_time", "1", "-hls_playlist_type", "vod",
		filepath.Join(dir, "stream.m3u8"))
	if out, err := generate.CombinedOutput(); err != nil {
		t.Skipf("ffmpeg cannot generate a test stream: %v\n%s", err, out)
	}
	edge := httptest.NewServer(http.FileServer(http.Dir(dir)))
	defer edge.Close()
	router := httptest.NewServer(http.RedirectHandler(edge.URL+"/stream.m3u8", http.StatusFound))
	defer router.Close()

	remuxer, err := New()
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := remuxer.Copy(t.Context(), &out, router.URL+"/live.m3u8", router.Client()); err != nil {
		t.Fatal(err)
	}

	// MPEG-TS is a run of 188-byte packets, each starting with the sync byte 0x47.
	if out.Len() == 0 || out.Len()%188 != 0 || out.Bytes()[0] != 0x47 {
		t.Errorf("Copy() wrote %d bytes that do not look like MPEG-TS", out.Len())
	}
}
