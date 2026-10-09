// Package httpclient fetches segments over HTTP/1.1, so giving one up closes
// only its own connection, not an HTTP/2 one the playlists share.
package httpclient

import "net/http"

type transport struct {
	base, http1 *http.Transport
}

// NewTransport returns base with an HTTP/1.1 pool beside it for segments.
func NewTransport(base *http.Transport) http.RoundTripper {
	return &transport{base: base, http1: segmentPool(base)}
}

// Wrap returns a transport that passes every request, segments too, to send.
func Wrap(next http.RoundTripper, send func(req *http.Request, next http.RoundTripper) (*http.Response, error)) http.RoundTripper {
	return &wrapped{through{next, send}}
}

// SegmentClient returns a copy of client on its segment pool, and a func that
// releases the pool.
func SegmentClient(client *http.Client) (*http.Client, func()) {
	segments := *client
	var release func()
	segments.Transport, release = segmentTransport(client.Transport)
	return &segments, release
}

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

type through struct {
	next http.RoundTripper
	send func(*http.Request, http.RoundTripper) (*http.Response, error)
}

func (t *through) RoundTrip(req *http.Request) (*http.Response, error) {
	return t.send(req, t.next)
}

// wrapped's segment transport is a plain through: it has no pool to hand out.
type wrapped struct{ through }

func (w *wrapped) segments() (http.RoundTripper, func()) {
	next, release := segmentTransport(w.next)
	return &through{next, w.send}, release
}
