// Package provider defines the contract a TV source implements to plug into
// telesfor.
//
// To add a source, implement Provider in a package under internal/provider and
// add it to the list in cmd/telesfor/main.go.
package provider

import (
	"context"
	"net/http"
	"time"
)

// Provider is a source of live TV channels, such as a broadcaster's streaming
// service.
type Provider interface {
	// Name is a short, URL-safe identifier, such as "tvp".
	Name() string

	// Channels lists the channels the provider can stream. It is called once,
	// at startup.
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
}

// Programme is an entry in the TV guide.
type Programme struct {
	ChannelID   string
	Title       string
	Description string
	Start, Stop time.Time
}

// Source is a live stream: the URL of a manifest ffmpeg can read, such as an
// HLS playlist, and the HTTP client to fetch it with. The client carries
// whatever it takes to reach the provider's streams, such as a proxy.
type Source struct {
	URL    string
	Client *http.Client
}
