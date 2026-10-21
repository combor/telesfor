package provider

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/combor/telesfor/internal/httpclient"
)

// How a server of the fake CDN answers.
const (
	serving = iota
	refusing
	failing
	missing
)

// origin is a server of the fake CDN, over TLS and HTTP/2. Its /slow sends
// half of the picture, then the rest once hold is closed, whatever it does
// with the rest: as a server that hands out a stream's playlists and refuses
// its segments.
type origin struct {
	*httptest.Server
	hold    chan struct{}
	release func()

	mu   sync.Mutex
	mode int
}

func newOrigin(t *testing.T, mode int) *origin {
	t.Helper()
	o := &origin{mode: mode, hold: make(chan struct{})}
	o.release = sync.OnceFunc(func() { close(o.hold) })
	o.Server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		o.mu.Lock()
		mode := o.mode
		o.mu.Unlock()
		switch {
		case r.URL.Path == "/slow":
			io.WriteString(w, "pic")
			w.(http.Flusher).Flush()
			<-o.hold
			io.WriteString(w, "ture")
		case mode == refusing:
			w.Header().Set("X-Refused", "here")
			w.WriteHeader(http.StatusForbidden)
			io.WriteString(w, "refused")
		case mode == failing:
			http.Error(w, "busy", http.StatusServiceUnavailable)
		case mode == missing:
			http.Error(w, "not found", http.StatusNotFound)
		default:
			io.WriteString(w, "picture")
		}
	}))
	o.EnableHTTP2 = true
	o.StartTLS()
	t.Cleanup(o.Close)
	t.Cleanup(o.release)
	return o
}

func (o *origin) set(mode int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.mode = mode
}

// cdn is a fake CDN behind a fake proxy. A resolver near the proxy has the
// name example.com at 192.0.2.1 and 192.0.2.2, and the proxy picks a third
// server of its own for it.
type cdn struct {
	proxy          string
	picked, b1, b2 *origin // the proxy's pick, and the servers at 192.0.2.1 and 192.0.2.2
	mu             sync.Mutex
	at             map[string]*origin // by the host the proxy is asked to connect to
	connected      []string           // the tunnels asked for
	forwarded      []string           // the requests the proxy sent on itself
	lookedUp       int
}

// newCDN starts a fake CDN whose servers all refuse, but for the one at
// 192.0.2.2.
func newCDN(t *testing.T) *cdn {
	t.Helper()
	c := &cdn{picked: newOrigin(t, refusing), b1: newOrigin(t, refusing), b2: newOrigin(t, serving)}
	c.at = map[string]*origin{"example.com": c.picked, "192.0.2.1": c.b1, "192.0.2.2": c.b2}
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Proxy-Authorization") != "Basic dXNlcjpzZWNyZXQ=" { // user:secret
			w.WriteHeader(http.StatusProxyAuthRequired)
			return
		}
		if r.Method != http.MethodConnect {
			c.mu.Lock()
			c.forwarded = append(c.forwarded, r.Method+" "+r.URL.String())
			c.mu.Unlock()
			w.Header().Set("X-Refused", "here")
			w.WriteHeader(http.StatusForbidden)
			return
		}
		host, _, _ := net.SplitHostPort(r.Host)
		c.mu.Lock()
		c.connected = append(c.connected, r.Host)
		server := c.at[host]
		c.mu.Unlock()
		if server == nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		upstream, err := net.Dial("tcp", server.Listener.Addr().String())
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		conn, _, _ := w.(http.Hijacker).Hijack()
		io.WriteString(conn, "HTTP/1.1 200 Connection established\r\n\r\n")
		go func() { io.Copy(upstream, conn); upstream.Close() }()
		io.Copy(conn, upstream)
		conn.Close()
	}))
	t.Cleanup(proxy.Close)
	c.proxy = "http://user:secret@" + proxy.Listener.Addr().String()
	return c
}

// client returns a client that reaches the CDN through the proxy, and the
// servers it chooses between.
func (c *cdn) client(t *testing.T) (*http.Client, *servers) {
	t.Helper()
	proxy, _ := url.Parse(c.proxy)
	base := http.DefaultTransport.(*http.Transport).Clone()
	base.TLSClientConfig = c.picked.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	refused := func(resp *http.Response) bool {
		return resp.StatusCode == http.StatusForbidden && resp.Header.Get("X-Refused") == "here"
	}
	s := newServers(base, proxy, refused, func(_ context.Context, host string) ([]string, error) {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.lookedUp++
		return []string{"192.0.2.1", "192.0.2.2"}, nil
	})
	client := &http.Client{Transport: s.transport}
	t.Cleanup(client.CloseIdleConnections)
	return client, s
}

// route has the proxy connect an address to a server, or to none.
func (c *cdn) route(address string, server *origin) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at[address] = server
}

// tunnels returns the tunnels asked for since it was last called, and how
// often the name has been looked up so far.
func (c *cdn) tunnels() ([]string, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	connected := c.connected
	c.connected = nil
	return connected, c.lookedUp
}

func (c *cdn) wantTunnels(t *testing.T, want []string, wantLookups int) {
	t.Helper()
	if got, lookups := c.tunnels(); !slices.Equal(got, want) || lookups != wantLookups {
		t.Errorf("tunnels %q after %d lookups, want %q after %d", got, lookups, want, wantLookups)
	}
}

// get fetches path from the CDN and returns the answer, with its protocol.
func get(t *testing.T, client *http.Client, path string) (status int, body string, major int) {
	t.Helper()
	resp, err := client.Get("https://example.com" + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(b), resp.ProtoMajor
}

func wantPicture(t *testing.T, client *http.Client, why string) {
	t.Helper()
	if status, body, _ := get(t, client, "/segment.ts"); status != http.StatusOK || body != "picture" {
		t.Errorf("%s: got %d %q, want the picture", why, status, body)
	}
}

func TestServers(t *testing.T) {
	c := newCDN(t)
	client, _ := c.client(t)
	segments, release := httpclient.SegmentClient(client)
	defer release()

	wantPicture(t, client, "the server the proxy picks refuses, and so does the next")
	c.wantTunnels(t, []string{"example.com:443", "192.0.2.1:443", "192.0.2.2:443"}, 1)

	// The name stays with the server that answered, segments too.
	for client, want := range map[*http.Client]int{client: 2, segments: 1} {
		if status, body, major := get(t, client, "/segment.ts"); status != http.StatusOK || body != "picture" || major != want {
			t.Errorf("got %d %q over HTTP/%d, want the picture over HTTP/%d", status, body, major, want)
		}
	}
	c.wantTunnels(t, []string{"192.0.2.2:443", "192.0.2.2:443"}, 1)

	// A segment on its way when that server refuses in its turn.
	resp, err := segments.Get("https://example.com/slow")
	if err != nil {
		t.Fatal(err)
	}
	c.b2.set(refusing)
	c.b1.set(serving)
	// The refusal comes over the connection to the chosen server, which is
	// not asked again.
	wantPicture(t, client, "the chosen server refuses in its turn")
	c.wantTunnels(t, []string{"192.0.2.1:443"}, 2)
	c.b2.release()
	if body, err := io.ReadAll(resp.Body); err != nil || string(body) != "picture" {
		t.Errorf("the segment on its way: %q, %v, want the whole picture", body, err)
	}
	resp.Body.Close()
	wantPicture(t, segments, "a segment after the switch")
	if _, lookups := c.tunnels(); lookups != 2 {
		t.Errorf("looked up %d times, want no more", lookups)
	}

	// The chosen server goes: the proxy picks again.
	c.route("192.0.2.1", nil)
	c.b2.set(serving)
	client.CloseIdleConnections()
	wantPicture(t, client, "the chosen server has gone")
	c.wantTunnels(t, []string{"192.0.2.1:443", "example.com:443", "192.0.2.1:443", "192.0.2.2:443"}, 3)
}

// A transfer over HTTP/2 shares its connection with a request that is
// refused: the switch to another server leaves it be.
func TestServersSharedConnection(t *testing.T) {
	c := newCDN(t)
	client, _ := c.client(t)

	resp, err := client.Get("https://example.com/slow")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	wantPicture(t, client, "a request refused over the same connection")
	c.wantTunnels(t, []string{"example.com:443", "192.0.2.1:443", "192.0.2.2:443"}, 1)
	c.picked.release()
	if body, err := io.ReadAll(resp.Body); err != nil || string(body) != "picture" || resp.ProtoMajor != 2 {
		t.Errorf("the transfer over HTTP/%d: %q, %v, want the whole picture over HTTP/2", resp.ProtoMajor, body, err)
	}
}

func TestServersFailing(t *testing.T) {
	all := []string{"example.com:443", "192.0.2.1:443", "192.0.2.2:443"}
	tests := []struct {
		name        string
		b1, b2      int
		want        int
		wantBody    string
		wantTunnels []string
		nextFirst   string // the tunnel the next request asks for
	}{
		{"another fails, the next serves", failing, serving, http.StatusOK, "picture", all, "192.0.2.2:443"},
		{"another has no such thing", missing, serving, http.StatusNotFound, "not found\n", all[:2], "192.0.2.1:443"},
		{"the others refuse", refusing, refusing, http.StatusForbidden, "refused", all, "example.com:443"},
		{"the others fail or refuse", failing, refusing, http.StatusForbidden, "refused", all, "example.com:443"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := newCDN(t)
			c.b1.set(test.b1)
			c.b2.set(test.b2)
			client, _ := c.client(t)

			if status, body, _ := get(t, client, "/segment.ts"); status != test.want || body != test.wantBody {
				t.Errorf("got %d %q, want %d %q", status, body, test.want, test.wantBody)
			}
			c.wantTunnels(t, test.wantTunnels, 1)
			client.CloseIdleConnections()
			get(t, client, "/segment.ts")
			if got, _ := c.tunnels(); len(got) == 0 || got[0] != test.nextFirst {
				t.Errorf("the next request asked for tunnels %q, want %s first", got, test.nextFirst)
			}
		})
	}
}

func TestServersAtOnce(t *testing.T) {
	c := newCDN(t)
	client, _ := c.client(t)

	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() { wantPicture(t, client, "a request among others") })
	}
	wg.Wait()
	if _, lookups := c.tunnels(); lookups != 1 {
		t.Errorf("looked up %d times, want once: the others ask the server chosen", lookups)
	}
}

// A search for one name that stalls holds up no other.
func TestServersSearchPerName(t *testing.T) {
	c := newCDN(t)
	c.route("other.example.com", c.picked)
	client, s := c.client(t)
	entered, stalled := make(chan struct{}), make(chan struct{})
	lookUp := s.lookUp
	s.lookUp = func(ctx context.Context, host string) ([]string, error) {
		if host == "example.com" {
			close(entered)
			<-stalled
		}
		return lookUp(ctx, host)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		if resp, err := client.Get("https://example.com/segment.ts"); err == nil {
			resp.Body.Close()
		}
	}()
	<-entered
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://other.example.com/segment.ts", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "picture" {
		t.Errorf("another name while the search for one stalls: got %d %q, want the picture", resp.StatusCode, body)
	}
	close(stalled)
	<-done
}

func TestServersChoiceLapses(t *testing.T) {
	c := newCDN(t)
	client, s := c.client(t)
	s.keep = 0

	wantPicture(t, client, "the first request")
	c.tunnels()
	client.CloseIdleConnections()
	wantPicture(t, client, "a request after the choice has lapsed")
	c.wantTunnels(t, []string{"example.com:443", "192.0.2.1:443", "192.0.2.2:443"}, 2)
}

func TestServersLeaveAlone(t *testing.T) {
	c := newCDN(t)
	client, _ := c.client(t)

	// Not the refusal another server can make up for.
	c.picked.set(failing)
	if status, _, _ := get(t, client, "/segment.ts"); status != http.StatusServiceUnavailable {
		t.Errorf("got %d, want the 503 as it came", status)
	}
	c.picked.set(refusing)
	// What cannot be asked again.
	resp, err := client.Post("https://example.com/upload", "text/plain", strings.NewReader("data"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("a POST: got %d, want the refusal", resp.StatusCode)
	}
	c.wantTunnels(t, []string{"example.com:443"}, 0)

	// An http:// request goes to the proxy as Client sends it.
	resp, err = client.Get("http://example.com/segment.ts")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	c.wantTunnels(t, nil, 0)
	if want := []string{"GET http://example.com/segment.ts"}; resp.StatusCode != http.StatusForbidden || !slices.Equal(c.forwarded, want) {
		t.Errorf("an http:// request: got %d, the proxy sent on %q, want the refusal and %q", resp.StatusCode, c.forwarded, want)
	}
}

// A proxy that takes the connection and never answers is given up on.
func TestServersProxySilent(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			defer conn.Close()
		}
	}()
	proxy, _ := url.Parse("http://" + listener.Addr().String())
	s := newServers(http.DefaultTransport.(*http.Transport).Clone(), proxy, func(*http.Response) bool { return false }, nil)
	s.dialer.Timeout = 50 * time.Millisecond

	began := time.Now()
	if _, err := (&http.Client{Transport: s.transport}).Get("https://example.com/"); err == nil || time.Since(began) > 5*time.Second {
		t.Errorf("got %v after %s, want an error at once", err, time.Since(began))
	}
}

func TestLookUp(t *testing.T) {
	resolver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") != "application/dns-json" || r.URL.Query().Get("name") != "cdn.example" || r.URL.Query().Get("type") != "A" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		io.WriteString(w, `{"Status":0,"Answer":[
			{"name":"cdn.example","type":5,"TTL":5052,"data":"a1.cdn.example."},
			{"name":"a1.cdn.example","type":1,"TTL":20,"data":"192.0.2.1"},
			{"name":"a1.cdn.example","type":1,"TTL":20,"data":"10.0.0.1"},
			{"name":"a1.cdn.example","type":1,"TTL":20,"data":"127.0.0.1"},
			{"name":"a1.cdn.example","type":1,"TTL":20,"data":"169.254.169.254"},
			{"name":"a1.cdn.example","type":1,"TTL":20,"data":"192.0.2.2"}]}`)
	}))
	defer resolver.Close()

	addresses, err := lookUp(t.Context(), resolver.Client(), resolver.URL, "cdn.example")
	if want := []string{"192.0.2.1", "192.0.2.2"}; err != nil || !slices.Equal(addresses, want) {
		t.Errorf("lookUp() = %q, %v, want the addresses on the internet, %q", addresses, err, want)
	}
}
