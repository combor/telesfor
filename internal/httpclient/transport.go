// Package httpclient keeps the segments of a stream apart from the rest of a
// provider's traffic. A segment may be given up before it is all there, such
// as a slow one that is fetched again in a lower quality. It then closes the
// HTTP/1.1 connection it has to itself, instead of holding up the playlists
// on an HTTP/2 connection they share. A segment that is fetched to the end
// leaves its connection for the next.
package httpclient

import "net/http"

// transport is a provider's transport: base for the API, the playlists and
// the keys, and http1, a pool of HTTP/1.1 connections, for segments.
type transport struct {
	base, http1 *http.Transport
}

// NewTransport returns a transport that sends over base, with a pool of
// HTTP/1.1 connections beside it for segments.
func NewTransport(base *http.Transport) http.RoundTripper {
	return &transport{base: base, http1: segmentPool(base)}
}

// Wrap returns a transport that sends each request with send, which passes it
// on to next. Its segment transport does the same over next's segment pool, so
// a wrapper's handling of requests holds for segments too.
func Wrap(next http.RoundTripper, send func(req *http.Request, next http.RoundTripper) (*http.Response, error)) http.RoundTripper {
	return &wrapped{through{next, send}}
}

// SegmentClient returns a copy of client that sends over its transport's
// segment pool, and a func that closes the pool if SegmentClient made one.
func SegmentClient(client *http.Client) (*http.Client, func()) {
	segments := *client
	var release func()
	segments.Transport, release = segmentTransport(client.Transport)
	return &segments, release
}

// segmentTransport returns the transport to fetch segments over, and what to
// call once they are done with. A transport from NewTransport has its pool at
// hand, and one from Wrap uses that of the transport it wraps. A bare
// *http.Transport gets a pool of its own, which release closes. Any other
// transport is used as it is.
func segmentTransport(base http.RoundTripper) (http.RoundTripper, func()) {
	if base == nil {
		base = http.DefaultTransport
	}
	if t, ok := base.(interface {
		segments() (http.RoundTripper, func())
	}); ok {
		return t.segments()
	}
	if t, ok := base.(*http.Transport); ok {
		pool := segmentPool(t)
		return pool, pool.CloseIdleConnections
	}
	return base, func() {}
}

// segmentPool returns a copy of base that speaks HTTP/1.1 only.
func segmentPool(base *http.Transport) *http.Transport {
	http1 := base.Clone()
	http1.Protocols = new(http.Protocols)
	http1.Protocols.SetHTTP1(true)
	if http1.TLSClientConfig != nil {
		// A cloned TLS config can still advertise h2.
		http1.TLSClientConfig.NextProtos = []string{"http/1.1"}
	}
	if http1.MaxIdleConnsPerHost == 0 {
		// Four viewers, with audio, video and overlapping requests.
		http1.MaxIdleConnsPerHost = 16
	}
	return http1
}

func (t *transport) RoundTrip(req *http.Request) (*http.Response, error) {
	return t.base.RoundTrip(req)
}

func (t *transport) segments() (http.RoundTripper, func()) {
	return t.http1, func() {}
}

func (t *transport) CloseIdleConnections() {
	t.base.CloseIdleConnections()
	t.http1.CloseIdleConnections()
}

// through sends each request with send over next.
type through struct {
	next http.RoundTripper
	send func(*http.Request, http.RoundTripper) (*http.Response, error)
}

func (t *through) RoundTrip(req *http.Request) (*http.Response, error) {
	return t.send(req, t.next)
}

// wrapped is a transport from Wrap. Its segment transport is a through, which
// has no segment pool of its own to hand out: asked for one, it is used as it
// is.
type wrapped struct{ through }

func (w *wrapped) segments() (http.RoundTripper, func()) {
	next, release := segmentTransport(w.next)
	return &through{next, w.send}, release
}
