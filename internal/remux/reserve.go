package remux

import (
	"io"
	"log/slog"
	"time"
)

const (
	// minLead is how much of a stream with a short playlist has come by the
	// time its first byte is sent. It is about what the head start leaves of
	// the shortest segments around once the start is trimmed: six of two
	// seconds leave ten, which keeps Plex playing.
	minLead = 9 * time.Second

	// maxHeld is the most of a stream to hold back.
	maxHeld = 32 << 20
)

// reserve holds the start of an MPEG-TS stream back until minLead of it has
// come, if the stream's playlist is short, and passes the rest on as it
// comes.
//
// A playlist may hold fewer segments than the head start asks for: TV
// Cultura's holds three, of two seconds each. Joined at its oldest segment,
// such a stream begins with a few seconds in hand and stays that close to
// running out. Plex holds more than that back from its player, which then
// has nothing to play. The stream cannot be joined further back, so it starts
// later instead: the viewer waits for what the playlist lacked.
type reserve struct {
	w     io.Writer
	short func() bool // whether the stream's playlist is short: see relay
	held  []byte      // the stream so far, until it is sent
	read  int         // how much of held has been looked at
	first int64       // the clock at the start of the stream; -1 until it shows
	sent  bool        // everything is passed on from here
}

func newReserve(w io.Writer, short func() bool) *reserve {
	return &reserve{w: w, short: short, first: -1}
}

func (r *reserve) Write(p []byte) (int, error) {
	if r.sent {
		return r.w.Write(p)
	}
	r.held = append(r.held, p...)
	enough := !r.short() || len(r.held) >= maxHeld
	for ; !enough && r.read+packetSize <= len(r.held); r.read += packetSize {
		packet := r.held[r.read : r.read+packetSize]
		clock, ok := clockReference(packet)
		switch {
		case packet[0] != 0x47: // not MPEG-TS: there is no telling how much has come
			enough = true
		case !ok:
		case r.first < 0:
			r.first = clock
		default:
			// A clock that runs backwards has started anew, and tells no more.
			enough = clock < r.first || clock-r.first >= int64(minLead/time.Second)*clockRate
		}
	}
	if !enough {
		return len(p), nil
	}
	return len(p), r.flush()
}

// flush sends what is held back, as it must when a stream ends before minLead
// of it has come. Of a stream that never came there is nothing to send, which
// leaves it to the caller to say so.
func (r *reserve) flush() error {
	if r.sent || len(r.held) == 0 {
		return nil
	}
	slog.Debug("remux: stream is sent", "held", len(r.held))
	r.sent = true
	_, err := r.w.Write(r.held)
	r.held = nil
	return err
}
