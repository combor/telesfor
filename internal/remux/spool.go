package remux

import (
	"io"
	"sync"
)

// maxSegment is the most of a segment to hold for ffmpeg. A larger one
// is passed on as it comes, and fetched no faster than ffmpeg takes it.
const maxSegment = 16 << 20

// spool takes in a body as fast as it comes, whatever ffmpeg does with it,
// and passes it on from the start, at once or once all of it has come.
type spool struct {
	mu     sync.Mutex
	more   *sync.Cond
	held   []byte // what has come and is still to be passed on
	closed bool   // no more is to come
	err    error  // why not, if not for its end
}

func newSpool() *spool {
	s := &spool{}
	s.more = sync.NewCond(&s.mu)
	return s
}

func (s *spool) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// A body of no end is taken in no faster than it is passed on.
	for len(s.held) >= maxSegment && !s.closed {
		s.more.Wait()
	}
	if s.closed {
		return 0, io.ErrClosedPipe
	}
	s.held = append(s.held, p...)
	s.more.Broadcast()
	return len(p), nil
}

// close marks the end of what comes: of all of it, or for the given reason.
func (s *spool) close(err error) {
	s.mu.Lock()
	if !s.closed {
		s.closed, s.err = true, err
	}
	s.more.Broadcast()
	s.mu.Unlock()
}

// wait returns once all has come, with the reason if it did not.
func (s *spool) wait() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for !s.closed {
		s.more.Wait()
	}
	return s.err
}

// passOn writes what has come and what is still to come to w. It returns the
// reason if not all of it came, or if w took none.
func (s *spool) passOn(w io.Writer) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for {
		for len(s.held) == 0 && !s.closed {
			s.more.Wait()
		}
		if len(s.held) == 0 {
			return s.err
		}
		piece := s.held
		s.held = nil
		s.mu.Unlock()
		_, err := w.Write(piece)
		s.mu.Lock()
		s.more.Broadcast()
		if err != nil {
			return err
		}
	}
}
