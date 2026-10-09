// Package httpclient isolates fetches that may be abandoned from other traffic.
package httpclient

import "net/http"

type transport struct {
	base, http1 *http.Transport
}

// NewTransport keeps the ordinary and segment connection pools together.
func NewTransport(base *http.Transport) http.RoundTripper {
	return &transport{base: base, http1: segmentPool(base)}
}

// Wrap returns a transport that sends each request with send, which passes it
// on to next. Its segment transport does the same over next's segment pool, so
// a wrapper's handling of requests holds for segments too.
func Wrap(next http.RoundTripper, send func(req *http.Request, next http.RoundTripper) (*http.Response, error)) http.RoundTripper {
	return &wrapped{through{next, send}}
}

// SegmentTransport selects a segment pool and returns a release function for
// any pool it creates. A transport from Wrap keeps its request handling there.
// Other transports keep their original protocol and ownership.
func SegmentTransport(base http.RoundTripper) (http.RoundTripper, func()) {
	if base == nil {
		base = http.DefaultTransport
	}
	if t, ok := base.(interface {
		SegmentTransport() (http.RoundTripper, func())
	}); ok {
		return t.SegmentTransport()
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

func (t *transport) SegmentTransport() (http.RoundTripper, func()) {
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

func (w *wrapped) SegmentTransport() (http.RoundTripper, func()) {
	next, release := SegmentTransport(w.next)
	return &through{next, w.send}, release
}
