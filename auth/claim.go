package auth

import (
	"sync"
	"sync/atomic"
	"time"
)

// ClaimWindow gates the unauthenticated Adopt RPC. It opens iff the
// capsule has zero enrolled keys at construction, and closes on the
// first successful Adopt OR after a fixed timeout — whichever comes
// first. There is no re-open path at runtime: the operator must trigger
// the console-side RESET_AUTH procedure and reboot to wipe the keystore.
type ClaimWindow struct {
	open   atomic.Bool
	once   sync.Once
	timer  *time.Timer
	expiry time.Time
}

// NewClaimWindow returns a window that is open if startEnrolled == 0
// and that auto-closes after timeout. A non-positive timeout disables
// the timer (window stays open until the first Close call).
func NewClaimWindow(startEnrolled int, timeout time.Duration) *ClaimWindow {
	w := &ClaimWindow{}
	if startEnrolled > 0 {
		return w
	}
	w.open.Store(true)
	if timeout > 0 {
		w.expiry = time.Now().Add(timeout)
		w.timer = time.AfterFunc(timeout, w.Close)
	}
	return w
}

// Open reports whether Adopt is currently callable.
func (w *ClaimWindow) Open() bool { return w.open.Load() }

// Close shuts the window. Idempotent. Stops the auto-close timer if
// running so we don't fire after the goroutine has finished elsewhere.
func (w *ClaimWindow) Close() {
	w.once.Do(func() {
		w.open.Store(false)
		if w.timer != nil {
			w.timer.Stop()
		}
	})
}

// Expiry returns the wall-clock time at which the window will auto-
// close, or the zero value if the window opened with no timeout. Used
// by the boot banner to surface the deadline to the operator.
func (w *ClaimWindow) Expiry() time.Time { return w.expiry }
