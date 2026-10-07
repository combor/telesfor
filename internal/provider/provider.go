// Package provider defines the contract a TV source implements to plug into
// telesfor.
//
// To add a source, implement Provider in a package under internal/provider and
// give it a tuner in cmd/telesfor/main.go. The tuner comes with a tab on the
// settings page, where an Account has its sign-in and Settings bring a part of
// their own.
package provider

import (
	"context"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"time"

	"github.com/combor/telesfor/internal/httpclient"
)

// Provider is a source of live TV channels, such as a broadcaster's streaming
// service.
type Provider interface {
	// Name is a short, URL-safe identifier, such as "tvp".
	Name() string

	// Channels lists the channels the provider can stream. It is called at
	// startup, and again when an Account's channels change.
	Channels(ctx context.Context) ([]Channel, error)

	// Programmes returns the TV guide of the given channels between from and to.
	Programmes(ctx context.Context, channels []Channel, from, to time.Time) ([]Programme, error)

	// Stream resolves a channel to its live stream. It is called every time the
	// channel is tuned, so the stream it returns may be short-lived.
	Stream(ctx context.Context, channelID string) (Source, error)
}

// Channel is a live TV channel.
type Channel struct {
	ID   string // unique within the provider and stable over time
	Name string
	Logo string // URL of the channel's logo; optional

	// Place is where the channel stands in the provider's lineup, from 1, for
	// a provider that knows its channels beforehand. The channel then keeps
	// its number when another comes or goes. Zero leaves it to the order the
	// channels are listed in.
	Place int
}

// Programme is an entry in the TV guide.
type Programme struct {
	ChannelID   string
	Title       string
	Description string
	Image       string // URL of the programme's poster; optional
	Start, Stop time.Time
}

// Source is a live stream: the URL of a manifest ffmpeg can read, such as an
// HLS playlist, and the HTTP client to fetch it with. The client carries
// whatever it takes to reach the provider's streams, such as a proxy.
//
// A stream that comes in more than one quality is best given as its master
// playlist, with all of them in it: it is then played in the best quality
// that the connection keeps up with.
type Source struct {
	URL    string
	Client *http.Client
}

// Client returns the HTTP client for a provider's API and streams. When proxy
// is set, everything goes through that HTTP proxy. Without one, the proxy
// settings of the environment apply.
func Client(proxy string) (*http.Client, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if proxy != "" {
		proxyURL, err := url.Parse(proxy)
		if err != nil || proxyURL.Host == "" {
			return nil, fmt.Errorf("invalid proxy %q: want a URL like http://host:port", proxy)
		}
		transport.Proxy = http.ProxyURL(proxyURL)
	}
	return &http.Client{Transport: httpclient.NewTransport(transport), Timeout: time.Minute}, nil
}

// Account is a Provider that streams to an account. The user signs in on the
// provider's own site with a code telesfor shows them, so telesfor never sees
// a password.
type Account interface {
	Provider

	// SignIn asks the provider for a code and waits in the background for the
	// user to enter it.
	SignIn(ctx context.Context) error

	// SignOut forgets the account, or the code the user has yet to enter.
	SignOut() error

	// Login reports where signing in stands.
	Login() Login

	// OnChange sets what to call when the provider's channels have changed.
	OnChange(func())
}

// Login is where signing in to an Account stands.
type Login struct {
	State   LoginState
	Code    string    // while Pending: what the user enters at URL
	URL     string    // while Pending: where the user enters Code
	Expires time.Time // while Pending: when Code stops working
	Problem string    // what went wrong last, in words for the user; optional
}

// LoginState is a step of signing in.
type LoginState int

const (
	SignedOut LoginState = iota
	Pending              // waiting for the user to enter the code
	SignedIn
	Expired // the provider no longer accepts the sign-in: the user has to sign in again
)

// Settings is a Provider with settings of its own. It writes its part of the
// settings page itself, and takes what the user sends from there.
type Settings interface {
	Provider

	// SettingsHTML is the provider's part of its tab, in HTML that uses the
	// page's stylesheet. Its forms post to action. It is asked for every time
	// the page refreshes.
	SettingsHTML(action string) (template.HTML, error)

	// Configure takes what one of those forms sent. What went wrong is for
	// SettingsHTML to show: the page returns to the tab. The provider is then
	// asked for its channels again, in case the settings change them.
	Configure(ctx context.Context, form url.Values) error
}
