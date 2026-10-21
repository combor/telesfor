package provider

import (
	"bufio"
	"cmp"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/netip"
	"net/url"
	"sync"
	"time"

	"github.com/combor/telesfor/internal/httpclient"
)

const (
	// resolver looks names up from where the proxy exits: a CDN hands out
	// the servers near whoever asks.
	resolver = "https://cloudflare-dns.com/dns-query"

	// keep is how long a name stays with the server chosen for it. Then the
	// proxy picks again, as the CDN may have moved on.
	keep = 10 * time.Minute
)

// EdgeClient is Client for a provider whose CDN serves a name from servers
// that may not all agree, as on where the proxy exits. When refused says a
// server turned a request away, the request is asked again of the name's
// other servers, and the name stays a while with the first that answers it.
//
// It takes an http:// proxy, through which the name is looked up too, and
// chooses servers for https:// requests. With no proxy, or another kind, it
// is Client.
func EdgeClient(proxy string, refused func(*http.Response) bool) (*http.Client, error) {
	if proxy == "" {
		return Client(proxy)
	}
	proxyURL, err := parseProxy(proxy)
	if err != nil {
		return nil, err
	}
	if proxyURL.Scheme != "http" {
		return Client(proxy)
	}
	dns := http.DefaultTransport.(*http.Transport).Clone()
	dns.Proxy = http.ProxyURL(proxyURL)
	dnsClient := &http.Client{Transport: dns, Timeout: 10 * time.Second}
	s := newServers(http.DefaultTransport.(*http.Transport).Clone(), proxyURL, refused, func(ctx context.Context, host string) ([]string, error) {
		return lookUp(ctx, dnsClient, resolver, host)
	})
	return &http.Client{Transport: s.transport, Timeout: time.Minute}, nil
}

// servers chooses which of a name's servers the proxy connects to, once one
// has refused. An https:// request goes through a tunnel of its own making:
// to the name, which the proxy looks up, or to the address chosen for it.
type servers struct {
	proxy     *url.URL
	proxyAddr string // as host:port
	refused   func(*http.Response) bool
	lookUp    func(ctx context.Context, host string) ([]string, error)
	keep      time.Duration
	dialer    net.Dialer

	base      *http.Transport   // dials through dial
	transport http.RoundTripper // base, with its segment pool, through send
	closeIdle func()            // closes the idle connections of both

	mu      sync.Mutex
	chosen  map[string]choice        // by name, as host:port
	seeking map[string]chan struct{} // by name: held while another server is sought
}

// choice is the server a name is asked at, and since when.
type choice struct {
	address string
	since   time.Time
}

// tunnel is a connection through the proxy to a server.
type tunnel struct {
	net.Conn
	address string // empty for the server the proxy picked
}

func newServers(base *http.Transport, proxy *url.URL, refused func(*http.Response) bool, lookUp func(context.Context, string) ([]string, error)) *servers {
	s := &servers{
		proxy:     proxy,
		proxyAddr: net.JoinHostPort(proxy.Hostname(), cmp.Or(proxy.Port(), "80")),
		refused:   refused,
		lookUp:    lookUp,
		keep:      keep,
		dialer:    net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second},
		base:      base,
		chosen:    map[string]choice{},
		seeking:   map[string]chan struct{}{},
	}
	// An http:// request is left to the proxy, as Client leaves it.
	base.Proxy = func(req *http.Request) (*url.URL, error) {
		if req.URL.Scheme == "https" {
			return nil, nil
		}
		return proxy, nil
	}
	base.DialContext = s.dial
	pools := httpclient.NewTransport(base)
	s.closeIdle = pools.(interface{ CloseIdleConnections() }).CloseIdleConnections
	s.transport = httpclient.Wrap(pools, s.send)
	return s
}

// send sends req through next and, if a server refuses it, to the name's
// other servers, each over a connection of its own. The refusal stands if
// none of them answers.
func (s *servers) send(req *http.Request, next http.RoundTripper) (*http.Response, error) {
	if req.URL.Scheme != "https" || req.Body != nil && req.Body != http.NoBody {
		return next.RoundTrip(req)
	}
	var used *tunnel
	traced := req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
		GotConn: func(info httptrace.GotConnInfo) { used = tunnelOf(info.Conn) },
	}))
	resp, err := next.RoundTrip(traced)
	if err != nil {
		return nil, err
	}
	resp.Request = req
	if !s.refused(resp) {
		return resp, nil
	}

	ctx, host, name := req.Context(), req.URL.Hostname(), hostPort(req.URL)
	from := ""
	if used != nil {
		from = used.address
	}
	s.mu.Lock()
	seeking, ok := s.seeking[name]
	if !ok {
		seeking = make(chan struct{}, 1)
		s.seeking[name] = seeking
	}
	s.mu.Unlock()
	select {
	case seeking <- struct{}{}:
		defer func() { <-seeking }()
	case <-ctx.Done():
		return resp, nil
	}
	tried := map[string]bool{from: true}
	// The server chosen before, or while this waited, first.
	answer, address, err := s.ask(req, []string{s.at(name)}, tried)
	if answer == nil && ctx.Err() == nil {
		addresses, lookErr := s.lookUp(ctx, host)
		if lookErr != nil {
			err = fmt.Errorf("looking up the servers: %w", lookErr)
		} else {
			var askErr error
			answer, address, askErr = s.ask(req, addresses, tried)
			err = cmp.Or(askErr, err, errors.New("there is no other"))
		}
	}
	if answer == nil {
		if ctx.Err() == nil {
			slog.Warn("no other server answers", "host", host, "err", err)
		}
		return resp, nil
	}

	// The connection that refused would be refused again. Over HTTP/1.1 it
	// carries nothing else; over HTTP/2 it may, so it goes only if idle.
	resp.Body.Close()
	if used != nil && resp.ProtoMajor == 1 {
		used.Close()
	}
	if address == s.at(name) {
		s.closeIdle()
		return answer, nil
	}
	reason := "the server the proxy picked refused"
	if from != "" {
		reason = from + " refused"
	}
	slog.Info("server", "host", host, "to", address, "reason", reason)
	s.choose(name, address)
	return answer, nil
}

// ask asks req of the servers at addresses not tried yet, in turn, each over
// a connection of its own. It returns the first answer that is neither a
// refusal nor a server's failure, with the address it came from: a 404 is an
// answer.
func (s *servers) ask(req *http.Request, addresses []string, tried map[string]bool) (*http.Response, string, error) {
	var err error
	for _, address := range addresses {
		if req.Context().Err() != nil {
			break
		}
		if address == "" || tried[address] {
			continue
		}
		tried[address] = true
		answer, sendErr := s.through(address).RoundTrip(req)
		switch {
		case sendErr != nil:
			err = sendErr
		case s.refused(answer) || answer.StatusCode >= 500:
			answer.Body.Close()
			err = fmt.Errorf("%s answers %s", address, answer.Status)
		default:
			return answer, address, nil
		}
	}
	return nil, "", err
}

// at returns the address name is asked at, or "" for the proxy's choice.
func (s *servers) at(name string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.chosen[name]
	if time.Since(c.since) > s.keep {
		delete(s.chosen, name)
		return ""
	}
	return c.address
}

// choose has name asked at address for a while. The idle connections go, as
// some may be to a server that refuses; those in use finish what they carry.
func (s *servers) choose(name, address string) {
	s.mu.Lock()
	s.chosen[name] = choice{address, time.Now()}
	s.mu.Unlock()
	s.closeIdle()
}

// through returns a transport that asks the server at address, over a
// connection of its own.
func (s *servers) through(address string) http.RoundTripper {
	t := s.base.Clone()
	t.DisableKeepAlives = true
	t.DialContext = func(ctx context.Context, _, name string) (net.Conn, error) {
		_, port, _ := net.SplitHostPort(name)
		return s.connect(ctx, net.JoinHostPort(address, port))
	}
	return t
}

// dial connects to name, a host:port. That is the proxy itself for an
// http:// request; else a tunnel through it, to the server chosen for the
// name or to the one the proxy picks.
func (s *servers) dial(ctx context.Context, network, name string) (net.Conn, error) {
	if name == s.proxyAddr {
		return s.dialer.DialContext(ctx, network, name)
	}
	_, port, _ := net.SplitHostPort(name)
	for {
		address, target := s.at(name), name
		if address != "" {
			target = net.JoinHostPort(address, port)
		}
		conn, err := s.connect(ctx, target)
		if err == nil {
			return &tunnel{conn, address}, nil
		}
		if address == "" || ctx.Err() != nil {
			return nil, err
		}
		// The server is gone: back to the proxy's choice.
		s.mu.Lock()
		if s.chosen[name].address == address {
			delete(s.chosen, name)
		}
		s.mu.Unlock()
	}
}

// connect asks the proxy for a tunnel to target, a host:port. The proxy has
// as long to answer as to take the connection: net/http dials on after a
// request is given up.
func (s *servers) connect(ctx context.Context, target string) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, s.dialer.Timeout)
	defer cancel()
	conn, err := s.dialer.DialContext(ctx, "tcp", s.proxyAddr)
	if err != nil {
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { conn.SetDeadline(time.Unix(1, 0)) })
	req := &http.Request{Method: http.MethodConnect, URL: &url.URL{Opaque: target}, Host: target, Header: http.Header{}}
	if user := s.proxy.User; user != nil {
		password, _ := user.Password()
		req.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(user.Username()+":"+password)))
	}
	if err = req.Write(conn); err == nil {
		reader := bufio.NewReader(conn)
		var resp *http.Response
		if resp, err = http.ReadResponse(reader, req); err == nil && resp.StatusCode != http.StatusOK {
			err = fmt.Errorf("the proxy answers %s for %s", resp.Status, target)
		} else if err == nil && reader.Buffered() > 0 {
			err = errors.New("the proxy sent more than its answer")
		}
	}
	if !stop() && err == nil {
		err = ctx.Err()
	}
	if err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

// tunnelOf returns the tunnel under conn, if it is one.
func tunnelOf(conn net.Conn) *tunnel {
	if tlsConn, ok := conn.(*tls.Conn); ok {
		conn = tlsConn.NetConn()
	}
	t, _ := conn.(*tunnel)
	return t
}

// hostPort returns the host:port a transport dials for u.
func hostPort(u *url.URL) string {
	port := u.Port()
	if port == "" {
		port = "443"
		if u.Scheme == "http" {
			port = "80"
		}
	}
	return net.JoinHostPort(u.Hostname(), port)
}

// lookUp asks server, a DNS-over-HTTPS resolver, for host's addresses on the
// internet: IPv4 ones, as a proxy's exit mostly has no other.
func lookUp(ctx context.Context, client *http.Client, server, host string) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server+"?"+url.Values{"name": {host}, "type": {"A"}}.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/dns-json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s answers %s", server, resp.Status)
	}
	var answer struct {
		Answer []struct {
			Type int
			Data string
		}
	}
	if err := json.NewDecoder(resp.Body).Decode(&answer); err != nil {
		return nil, err
	}
	var addresses []string
	for _, record := range answer.Answer {
		// Type 1 is an address; the others are the names on the way.
		if addr, err := netip.ParseAddr(record.Data); record.Type == 1 && err == nil && addr.Is4() && addr.IsGlobalUnicast() && !addr.IsPrivate() {
			addresses = append(addresses, record.Data)
		}
	}
	return addresses, nil
}
