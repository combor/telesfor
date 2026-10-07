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

// SegmentTransport selects a segment pool and returns a release function for
// any pool it creates. Wrappers implement the same method to preserve their
// request handling. Other transports keep their original protocol and ownership.
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
