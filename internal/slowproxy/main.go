// Command slowproxy is an HTTP proxy that lets through only so many bits a
// second, for trying telesfor on a connection that is slower than the real
// one, or that changes:
//
//	go run ./internal/slowproxy -rate 20M
//	telesfor -tvp-proxy http://127.0.0.1:8899
//
// The rate is for everything that comes through together, as on a real
// connection, and can be set while the proxy runs:
//
//	curl 'http://127.0.0.1:8899/rate?to=3M'
//
// It tunnels, as a proxy does for HTTPS, which is what the providers speak.
//
// Behind another proxy, which -via names, it is less true to life: what that
// proxy has taken in still has to come through after a transfer is given up.
//
// It is a tool for development, and no part of telesfor.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// link is the connection that everything shares: so many bits a second.
type link struct {
	mu   sync.Mutex
	rate float64   // in bits a second; 0 for no limit
	free time.Time // when what was let through so far has passed
}

func (l *link) set(rate float64) {
	l.mu.Lock()
	l.rate, l.free = rate, time.Now()
	l.mu.Unlock()
	log.Printf("rate: %s", megabits(rate))
}

// pass waits until n more bytes may go through.
func (l *link) pass(n int) {
	l.mu.Lock()
	now := time.Now()
	if l.rate <= 0 || l.free.Before(now) {
		l.free = now
	}
	wait := l.free.Sub(now)
	if l.rate > 0 {
		l.free = l.free.Add(time.Duration(float64(n) * 8 / l.rate * float64(time.Second)))
	}
	l.mu.Unlock()
	time.Sleep(wait)
}

// copy passes what comes from the far end on at the link's rate.
func (l *link) copy(to io.Writer, from io.Reader) {
	buf := make([]byte, 4<<10) // small pieces, so that the rate is even
	for {
		n, err := from.Read(buf)
		if n > 0 {
			l.pass(n)
			if _, err := to.Write(buf[:n]); err != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

func megabits(rate float64) string {
	if rate <= 0 {
		return "no limit"
	}
	return fmt.Sprintf("%.2fMbps", rate/1e6)
}

// parseRate reads a rate such as 3M, 800k or 0 for no limit.
func parseRate(s string) (float64, error) {
	scale := 1.0
	switch {
	case strings.HasSuffix(s, "M"):
		s, scale = strings.TrimSuffix(s, "M"), 1e6
	case strings.HasSuffix(s, "k"):
		s, scale = strings.TrimSuffix(s, "k"), 1e3
	}
	rate, err := strconv.ParseFloat(s, 64)
	if err != nil || rate < 0 {
		return 0, fmt.Errorf("bad rate %q: want a number of bits a second, such as 3M or 800k", s)
	}
	return rate * scale, nil
}

// proxy is the HTTP proxy.
type proxy struct {
	link link
	via  *url.URL // the proxy to go on through, if any
}

func (p *proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodConnect:
	case !r.URL.IsAbs() && r.URL.Path == "/rate":
		rate, err := parseRate(r.URL.Query().Get("to"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		p.link.set(rate)
		fmt.Fprintln(w, megabits(rate))
		return
	default:
		http.Error(w, "slowproxy only tunnels: ask for an https address", http.StatusNotImplemented)
		return
	}

	far, reader, err := p.dial(r.Host)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer far.Close()
	near, _, err := w.(http.Hijacker).Hijack()
	if err != nil {
		return
	}
	defer near.Close()
	io.WriteString(near, "HTTP/1.1 200 Connection established\r\n\r\n")
	go func() {
		io.Copy(far, near) // what is sent is small, and goes as it comes
		far.Close()
	}()
	p.link.copy(near, reader)
}

// dial connects to a host, through the next proxy if there is one. It returns
// the connection and what to read from it.
//
// The connection takes in little at a time. A slow line has little on its way
// at any moment, and so little left to come once a transfer is given up: with
// megabytes waiting here to be let through, giving up would free nothing.
func (p *proxy) dial(host string) (net.Conn, io.Reader, error) {
	to := host
	if p.via != nil {
		to = p.via.Host
	}
	conn, err := net.DialTimeout("tcp", to, 15*time.Second)
	if err != nil {
		return nil, nil, err
	}
	if tcp, ok := conn.(*net.TCPConn); ok {
		tcp.SetReadBuffer(256 << 10)
	}
	if p.via == nil {
		return conn, conn, nil
	}
	fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", host, host)
	// What the next proxy says of the length of its answer means nothing:
	// after its headers comes the tunnel.
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, &http.Request{Method: http.MethodConnect})
	if err == nil && resp.StatusCode != http.StatusOK {
		err = fmt.Errorf("%s answers %s", p.via.Host, resp.Status)
	}
	if err != nil {
		conn.Close()
		return nil, nil, err
	}
	return conn, reader, nil
}

func main() {
	listen := flag.String("listen", "127.0.0.1:8899", "address to listen on")
	via := flag.String("via", "", "HTTP proxy to go on through, such as http://127.0.0.1:8889")
	rate := flag.String("rate", "0", "bits a second to let through, such as 3M or 800k; 0 for no limit")
	flag.Parse()

	limit, err := parseRate(*rate)
	if err != nil {
		log.Fatal(err)
	}
	p := &proxy{}
	if *via != "" {
		if p.via, err = url.Parse(*via); err != nil || p.via.Host == "" {
			log.Fatalf("bad proxy %q: want a URL like http://host:port", *via)
		}
	}
	p.link.set(limit)
	log.Printf("listening on %s", *listen)
	log.Fatal(http.ListenAndServe(*listen, p))
}
