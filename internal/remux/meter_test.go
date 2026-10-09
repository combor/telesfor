package remux

import "testing"

// A stream starts over only while none of it has gone to the viewer, and none
// of what came before goes then.
func TestMeterStartsOver(t *testing.T) {
	var out writes
	m := newMeter(&out)
	packet := make([]byte, packetSize)
	if !m.startOver() {
		t.Fatal("a stream of which nothing has gone cannot start over")
	}
	m.Write(packet)
	if _, _, onAir := m.sent(); onAir || len(out) != 0 {
		t.Errorf("%d writes went to the viewer of a stream that is starting over, want none", len(out))
	}
	m.started()
	m.Write(packet)
	if _, _, onAir := m.sent(); !onAir || len(out) != 1 {
		t.Errorf("%d writes went to the viewer of the stream as started over, want the one", len(out))
	}
	if m.startOver() {
		t.Error("a stream that has gone to the viewer starts over")
	}
	if m.Write(packet); len(out) != 2 {
		t.Errorf("%d writes went to the viewer, want all that came since it could not start over", len(out))
	}
}
