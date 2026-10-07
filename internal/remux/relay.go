package remux

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"path"
	"sync"
	"sync/atomic"
	"time"
)

// relay lets ffmpeg read one stream through the provider's HTTP client.
//
// Every upstream server the stream touches gets a twin: a loopback listener
// that answers each request by fetching the same path from the server it
// stands in for. ffmpeg is handed the manifest's URL on a twin, and from there
// on behaves as it would against the real servers. The relative URIs in a
// manifest lead back to the twin it came from, and a redirect is rewritten to
// lead to the twin of its target. Only a manifest that names another server by
// full URL leads ffmpeg away from the relay.
//
// Files are passed on as they are, but for MPEG-TS with timestamps that cannot
// be right: those are repaired on the way. See repairDTS. A playlist is looked
// at as it passes, for how many segments it holds. See reserve.
//
// A stream that comes in more than one quality is not read through the twins:
// see stage.
type relay struct {
	client *http.Client // fetches what ffmpeg asks a twin for, and leaves redirects to ffmpeg
	direct *http.Client // fetches the rest, and follows them
	late   atomic.Int64 // how late the stream stamps its frames to be decoded: see repairDTS
	short  atomic.Bool  // the stream's playlist holds fewer segments than headStart: see reserve

	mu     sync.Mutex
	twins  map[string]*http.Server // by the server they stand in for, as scheme://host
	kept   *kept                   // the manifest, until ffmpeg asks for it: see qualities
	closed bool
}

// kept is a file that was fetched before ffmpeg asked for it.
type kept struct {
	address string // where it was found, after redirects
	kind    string // its Content-Type
	body    []byte
}

// openRelay starts a relay for the stream behind the manifest URL. It returns
// the relay and the local URL to read the manifest from.
func openRelay(manifest string, client *http.Client) (r *relay, local string, err error) {
	upstream, err := url.Parse(manifest)
	if err != nil {
		return nil, "", err
	}
	if client == nil {
		client = http.DefaultClient
	}
	// ffmpeg has to see the redirects itself: they change what the relative
	// URIs in a playlist refer to.
	noFollow := *client
	noFollow.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	r = &relay{client: &noFollow, direct: client, twins: map[string]*http.Server{}}
	local, err = r.local(upstream)
	if err != nil {
		return nil, "", err
	}
	return r, local, nil
}

// local returns the URL on a twin that stands for an upstream URL. It starts
// the twin of the URL's server if there is none yet.
func (r *relay) local(upstream *url.URL) (string, error) {
	server := upstream.Scheme + "://" + upstream.Host

	r.mu.Lock()
	defer r.mu.Unlock()
	twin, ok := r.twins[server]
	if !ok {
		if r.closed {
			return "", errors.New("relay is closed")
		}
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return "", err
		}
		twin = &http.Server{
			Addr: listener.Addr().String(), // only kept here to build local URLs from
			Handler: http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				r.fetch(w, req, server)
			}),
		}
		go twin.Serve(listener)
		r.twins[server] = twin
	}
	return "http://" + twin.Addr + upstream.RequestURI(), nil
}

// qualities fetches the stream's manifest and returns the qualities it lists,
// if it is a master playlist with more than one to go between. Any other
// manifest is kept for ffmpeg, which is about to ask for it: once, as a live
// playlist is another the next time.
func (r *relay) qualities(ctx context.Context, manifest string) *ladder {
	fetch, err := http.NewRequestWithContext(ctx, http.MethodGet, manifest, nil)
	if err != nil {
		return nil
	}
	resp, err := r.direct.Do(fetch)
	if err != nil {
		return nil // ffmpeg will meet the same, and Copy will say so
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxPlaylist))
	if err != nil || resp.StatusCode != http.StatusOK {
		return nil
	}
	if l := parseMaster(string(body), resp.Request.URL); l != nil {
		return l
	}
	r.mu.Lock()
	r.kept = &kept{resp.Request.URL.String(), resp.Header.Get("Content-Type"), body}
	r.mu.Unlock()
	return nil
}

// fetch answers a request to a twin with the same path fetched from server.
func (r *relay) fetch(w http.ResponseWriter, req *http.Request, server string) {
	began := time.Now()
	file := path.Base(req.URL.Path) // for the log: the rest of the path may carry the stream's token

	r.mu.Lock()
	kept := r.kept
	if kept != nil && kept.address == server+req.URL.RequestURI() {
		r.kept = nil
	} else {
		kept = nil
	}
	r.mu.Unlock()
	if kept != nil {
		if kept.kind != "" {
			w.Header().Set("Content-Type", kept.kind)
		}
		w.Write(kept.body)
		r.count(kept.body)
		return
	}

	fetch, err := http.NewRequestWithContext(req.Context(), http.MethodGet, server+req.URL.RequestURI(), nil)
	if err != nil {
		http.Error(w, "bad upstream URL", http.StatusBadGateway)
		return
	}
	if byteRange := req.Header.Get("Range"); byteRange != "" {
		fetch.Header.Set("Range", byteRange)
	}
	resp, err := r.client.Do(fetch)
	if err != nil {
		// A cancelled request only means the stream was closed. For the rest,
		// Unwrap drops the URL, and with it the stream's token, from the log.
		if req.Context().Err() == nil {
			slog.Warn("relay: upstream request failed", "file", file, "err", errors.Unwrap(err))
		}
		http.Error(w, "upstream request failed", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= http.StatusBadRequest {
		slog.Warn("relay: upstream refused", "file", file, "status", resp.Status)
	}
	if target, err := resp.Location(); err == nil { // a redirect
		location, err := r.local(target)
		if err != nil {
			http.Error(w, "cannot follow upstream redirect", http.StatusBadGateway)
			return
		}
		w.Header().Set("Location", location)
	}
	passHeaders(w, resp)
	var playlist bytes.Buffer
	if path.Ext(req.URL.Path) == ".m3u8" {
		resp.Body = io.NopCloser(io.TeeReader(resp.Body, &playlist))
	}
	size, err := r.pass(w, resp)
	if err != nil {
		if req.Context().Err() == nil {
			slog.Warn("relay: upstream transfer failed", "file", file, "err", err)
		}
		// Cutting the connection tells ffmpeg that the file is incomplete.
		// Ending the response normally would pass the part off as the whole.
		panic(http.ErrAbortHandler)
	}
	r.count(playlist.Bytes())
	slog.Debug("relay: fetched", "file", file, "status", resp.StatusCode, "bytes", size, "took", time.Since(began).Round(time.Millisecond))
}

// count notes whether a playlist holds fewer segments than the head start
// asks for. A master playlist lists none, and says nothing of how many the
// stream's own holds.
func (r *relay) count(playlist []byte) {
	if segments := bytes.Count(playlist, []byte("#EXTINF")); segments > 0 {
		r.short.Store(segments < headStart)
	}
}

// pass copies the body of a response to w. MPEG-TS comes in packets of 188
// bytes, so a body that may be made of them is passed on in whole packets,
// with their timestamps repaired.
func (r *relay) pass(w io.Writer, resp *http.Response) (size int64, err error) {
	if resp.ContentLength <= 0 || resp.ContentLength%packetSize != 0 {
		return io.Copy(w, resp.Body)
	}

	was := r.late.Load()
	buf := make([]byte, 32<<10)
	held := 0 // bytes at the start of buf: a packet that has not arrived in full
	for err == nil {
		var n int
		n, err = resp.Body.Read(buf[held:])
		whole := (held + n) / packetSize * packetSize
		repairDTS(buf[:whole], &r.late)
		if _, err := w.Write(buf[:whole]); err != nil {
			return size, err
		}
		size += int64(whole)
		held = copy(buf, buf[whole:held+n])
	}
	if late := r.late.Load(); late != was {
		slog.Debug("relay: decoding times run late, moving them back", "by", time.Duration(late)*time.Second/90000)
	}
	if err == io.EOF {
		err = nil
	}
	return size, err
}

// close stops the relay's twins.
func (r *relay) close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	for _, twin := range r.twins {
		twin.Close()
	}
}
