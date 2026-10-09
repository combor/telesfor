package provider

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Rest is how long a stream's pass is left alone once it is handed out: one
// this new is not refused for its age.
const Rest = time.Minute

// Pass keeps good the pass that starts the path of every address of a stream.
// The stream goes on being asked for with the one it started with, so each
// request is sent with the newest. The provider tells which refusals call for
// a new one.
type Pass struct {
	fresh func(context.Context) string // gets a new pass, or none
	rest  time.Duration

	mu     sync.Mutex
	first  string    // the pass in the addresses ffmpeg asks for; empty if they carry none
	pass   string    // the pass to ask with
	signed time.Time // when it was handed out
}

// NewPass returns the Pass of a stream whose addresses carry first, which was
// just handed out.
func NewPass(first string, rest time.Duration, fresh func(context.Context) string) *Pass {
	return &Pass{fresh: fresh, rest: rest, first: first, pass: first, signed: time.Now()}
}

// Current returns the pass to ask with.
func (p *Pass) Current() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.pass
}

// Send sends req through next with pass in place of the first.
func (p *Pass) Send(next http.RoundTripper, req *http.Request, pass string) (*http.Response, error) {
	out := req.Clone(req.Context())
	if file, ok := strings.CutPrefix(out.URL.Path, p.first+"/"); ok && p.first != "" {
		out.URL.Path, out.URL.RawPath = pass+"/"+file, ""
	}
	resp, err := next.RoundTrip(out)
	if resp != nil {
		// The answer is to what was asked. What a playlist lists is then
		// asked for the same way, with the pass that is renewed.
		resp.Request = req
	}
	return resp, err
}

// Renew returns the pass to ask with after one was refused: a new one, or
// none if a new one will not help.
func (p *Pass) Renew(ctx context.Context, refused string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch {
	case p.first == "":
		return ""
	case p.pass != refused:
		return p.pass // another request has renewed it since
	case time.Since(p.signed) < p.rest:
		return ""
	}
	pass := p.fresh(ctx)
	if pass == "" {
		return ""
	}
	p.pass, p.signed = pass, time.Now()
	return pass
}
