// Package rotate is the credential generation clock: pure arithmetic over a
// timestamp, no clock resource, no provider, no state.
//
// Every minted credential exists as two overlapping generations, the
// current one and the one before, both valid. A replacement is always in
// place well before its predecessor dies, so nothing has to be restarted at
// the moment of a swap: at a 45-day period every token is at most 90 days
// old and every consumer has 45 days to pick up the new value on its own
// refresh.
//
// This is why the module cannot be a `time_rotating` resource. That clock
// resource can replace one token on a schedule, but a replaced token is
// dead the moment the apply runs, while CI and every pod still holding its
// predecessor keep using it for hours. The overlapping pair removes that
// window: nobody decides "it is time" -- the daily pass re-plans the
// credentials configuration and the plan is empty until the date crosses a
// generation boundary. On the day it does, applying it *is* the rotation.
// That only works if the same inputs produce byte-identical output on any
// machine, at any time, in any zone -- so the plan can be evaluated offline
// and two parties can agree about it without either running the other's
// clock.
package rotate

import (
	"fmt"
	"time"
)

// Clock is the rotation policy: an epoch and a period, and nothing else.
type Clock struct {
	Epoch  time.Time
	Period time.Duration
}

// Generation is the number of a generation and the window it is valid for.
type Generation struct {
	N      int
	Begins time.Time
	// Retires is when the generation stops being live: Begins + 2*Period.
	// It is live for two periods, not one, because Live always holds the
	// current generation and its immediate predecessor.
	Retires time.Time
}

// Name renders the generation number the way the vault and CI do: "g7".
// The "<name>-g<n>" form is the caller's concern, not this package's.
func (g Generation) Name() string {
	return fmt.Sprintf("g%d", g.N)
}

// Validate reports every problem with the clock, not just the first, the
// way internal/config.Load does -- so a misconfigured clock is diagnosed in
// one pass rather than one refusal at a time.
func (c Clock) Validate() []string {
	var problems []string
	if c.Epoch.IsZero() {
		problems = append(problems, "rotate: Epoch is zero")
	}
	// A zero or negative period would put every credential in generation 0
	// forever -- a rotation that never happens while reporting success, the
	// exact fail-open shape this project refuses.
	if c.Period <= 0 {
		problems = append(problems, "rotate: Period must be positive")
	}
	return problems
}

// utc normalises a time to UTC. All arithmetic here runs in UTC, and the
// epoch is normalised on every use, because a clock that depends on the
// machine's zone is a clock two machines disagree about -- and this whole
// design rests on them agreeing.
func utc(t time.Time) time.Time {
	return t.UTC()
}

// generationAt computes the generation number for at, given a clock already
// known to be valid, and reports whether at is before the epoch.
//
// Go's integer division truncates toward zero, so a naive (t-epoch)/period
// yields 0 for any t in the period *before* the epoch, and -1 only once t is
// a full period earlier than that -- meaning a misconfigured epoch in the
// future would silently produce the same generation as the epoch itself.
// That is why "before the epoch" is checked explicitly rather than left to
// fall out of the division.
func generationAt(epoch time.Time, period time.Duration, at time.Time) (int, bool) {
	epoch = utc(epoch)
	at = utc(at)
	if at.Before(epoch) {
		return 0, false
	}
	elapsed := at.Sub(epoch)
	return int(elapsed / period), true
}

// Current returns the generation live at at. It is an error for at to be
// before Epoch: that is a misconfiguration, not generation 0.
func (c Clock) Current(at time.Time) (Generation, error) {
	if problems := c.Validate(); len(problems) > 0 {
		return Generation{}, fmt.Errorf("rotate: invalid clock: %v", problems)
	}
	n, ok := generationAt(c.Epoch, c.Period, at)
	if !ok {
		return Generation{}, fmt.Errorf("rotate: at (%s) is before epoch (%s)", utc(at), utc(c.Epoch))
	}
	begins := utc(c.Epoch).Add(time.Duration(n) * c.Period)
	return Generation{
		N:       n,
		Begins:  begins,
		Retires: begins.Add(2 * c.Period),
	}, nil
}

// Live returns the current generation and its predecessor, current first.
// It returns exactly one generation when the current one is g0: there is no
// predecessor before the first, and Live must never fabricate a g-1.
func (c Clock) Live(at time.Time) ([]Generation, error) {
	current, err := c.Current(at)
	if err != nil {
		return nil, err
	}
	if current.N == 0 {
		return []Generation{current}, nil
	}
	prevBegins := current.Begins.Add(-c.Period)
	previous := Generation{
		N:       current.N - 1,
		Begins:  prevBegins,
		Retires: prevBegins.Add(2 * c.Period),
	}
	return []Generation{current, previous}, nil
}

// NextBoundary returns the next instant a generation begins, at or after
// at.
//
// Exactly on a boundary, NextBoundary returns that same instant rather than
// the following one: a boundary instant already belongs to the generation
// it starts, so "next" means "the next one not yet reached", and the
// boundary at hand qualifies. TestNextBoundary pins this.
func (c Clock) NextBoundary(at time.Time) (time.Time, error) {
	if problems := c.Validate(); len(problems) > 0 {
		return time.Time{}, fmt.Errorf("rotate: invalid clock: %v", problems)
	}
	epoch := utc(c.Epoch)
	atUTC := utc(at)
	if atUTC.Before(epoch) {
		return time.Time{}, fmt.Errorf("rotate: at (%s) is before epoch (%s)", atUTC, epoch)
	}
	n, _ := generationAt(c.Epoch, c.Period, at)
	boundary := epoch.Add(time.Duration(n) * c.Period)
	if boundary.Equal(atUTC) {
		return boundary, nil
	}
	return boundary.Add(c.Period), nil
}
