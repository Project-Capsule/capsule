package auth

import (
	"testing"
	"time"
)

func TestClaimWindowOpensWhenZeroEnrolled(t *testing.T) {
	w := NewClaimWindow(0, time.Minute)
	if !w.Open() {
		t.Fatal("expected window open with zero enrolled")
	}
}

func TestClaimWindowClosedWhenAlreadyEnrolled(t *testing.T) {
	w := NewClaimWindow(1, time.Minute)
	if w.Open() {
		t.Fatal("expected window closed when keys already enrolled")
	}
}

func TestClaimWindowExplicitClose(t *testing.T) {
	w := NewClaimWindow(0, time.Minute)
	w.Close()
	if w.Open() {
		t.Fatal("expected closed after explicit Close")
	}
	// Idempotent.
	w.Close()
}

func TestClaimWindowAutoClose(t *testing.T) {
	w := NewClaimWindow(0, 50*time.Millisecond)
	if !w.Open() {
		t.Fatal("should be open at t=0")
	}
	time.Sleep(150 * time.Millisecond)
	if w.Open() {
		t.Fatal("should be closed after timeout")
	}
}

func TestClaimWindowDoesNotReopen(t *testing.T) {
	w := NewClaimWindow(0, time.Minute)
	w.Close()
	// Sleep a touch — if any background path were to flip it back,
	// it would have done so by now.
	time.Sleep(10 * time.Millisecond)
	if w.Open() {
		t.Fatal("expected window to stay closed")
	}
}
