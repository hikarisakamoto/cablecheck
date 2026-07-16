package clock

import "time"

// Real is the production Clock, backed by the wall clock via the time
// package. The zero value is ready to use.
type Real struct{}

var _ Clock = Real{}

// Now returns time.Now().
func (Real) Now() time.Time { return time.Now() }

// After returns time.After(d).
func (Real) After(d time.Duration) <-chan time.Time { return time.After(d) }

// NewTicker returns a Ticker wrapping time.NewTicker(d). It panics if d <= 0.
func (Real) NewTicker(d time.Duration) Ticker { return realTicker{time.NewTicker(d)} }

// realTicker adapts *time.Ticker to the Ticker interface.
type realTicker struct{ t *time.Ticker }

func (rt realTicker) C() <-chan time.Time { return rt.t.C }
func (rt realTicker) Stop()               { rt.t.Stop() }
