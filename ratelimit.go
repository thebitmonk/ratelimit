// Copyright (c) 2016 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package ratelimit // import "github.com/thebitmonk/ratelimit"

import (
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/thebitmonk/ratelimit/internal/clock"
)

// Note: This file is inspired by:
// https://github.com/prashantv/go-bench/blob/master/ratelimit

// Limiter is used to rate-limit some process, possibly across goroutines.
// The process is expected to call Take() before every iteration, which
// may block to throttle the goroutine.
type Limiter interface {
	// Take should block to make sure that the RPS is met.
	Take() time.Time
}

// Clock is the minimum necessary interface to instantiate a rate limiter with
// a clock or mock clock, compatible with clocks created using
// github.com/andres-erbsen/clock.
type Clock interface {
	Now() time.Time
	Sleep(time.Duration)
}

type state struct {
	last     time.Time
	sleepFor time.Duration
}

type limiter struct {
	state   unsafe.Pointer
	padding [56]byte // cache line size - state pointer size = 64 - 8; created to avoid false sharing

	perRequest time.Duration
	maxSlack   time.Duration
	clock      Clock
}

type options struct {
	interval time.Duration
	slack    int
	noSlack  bool
	clock    Clock
}

var defaultOptions = options{
	slack: 10,
}

// Option configures a Limiter.
type Option interface {
	apply(l *options)
}

type optionFunc func(*options)

func (f optionFunc) apply(options *options) {
	f(options)
}

// New returns a Limiter that will limit to the given takes per second.
func New(rate int, opts ...Option) Limiter {
	o := defaultOptions
	for _, opt := range opts {
		opt.apply(&o)
	}

	l := &limiter{}

	if o.noSlack {
		o.slack = 0
	}

	if o.interval == 0 {
		o.interval = time.Second
	}
	l.perRequest = o.interval / time.Duration(rate)
	l.maxSlack = -time.Duration(o.slack) * o.interval / time.Duration(rate)

	if o.clock == nil {
		o.clock = clock.New()
	}
	l.clock = o.clock

	initialState := state{
		last:     time.Time{},
		sleepFor: 0,
	}

	atomic.StorePointer(&l.state, unsafe.Pointer(&initialState))
	return l
}

// Per overrides the interval of the rate limit.
//
// The default interval is one second, so New(100) produces a one hundred per
// second (100 Hz) rate limiter.
//
// New(2, Per(60*time.Second)) creates a 2 per minute rate limiter.
//
// The interval must be overridden for any rate limit
func Per(interval time.Duration) Option {
	return optionFunc(func(o *options) {
		o.interval = interval
	})
}

// WithClock returns an option for ratelimit.New that provides an alternate
// Clock implementation, typically a mock Clock for testing.
func WithClock(clock Clock) Option {
	return optionFunc(func(o *options) {
		o.clock = clock
	})
}

// WithoutSlack is an option for ratelimit.New that initializes the limiter
// without any initial tolerance for bursts of traffic.
var WithoutSlack = optionFunc(func(o *options) {
	o.noSlack = true
})

// Take blocks to ensure that the time spent between multiple
// Take calls is on average time.Second/rate.
func (t *limiter) Take() time.Time {
	newState := state{}
	taken := false
	for !taken {
		now := t.clock.Now()

		previousStatePointer := atomic.LoadPointer(&t.state)
		oldState := (*state)(previousStatePointer)

		newState = state{}
		newState.last = now

		// If this is our first request, then we allow it.
		if oldState.last.IsZero() {
			taken = atomic.CompareAndSwapPointer(&t.state, previousStatePointer, unsafe.Pointer(&newState))
			continue
		}

		// sleepFor calculates how much time we should sleep based on
		// the perRequest budget and how long the last request took.
		// Since the request may take longer than the budget, this number
		// can get negative, and is summed across requests.
		newState.sleepFor += t.perRequest - now.Sub(oldState.last)
		// We shouldn't allow sleepFor to get too negative, since it would mean that
		// a service that slowed down a lot for a short period of time would get
		// a much higher RPS following that.
		if newState.sleepFor < t.maxSlack {
			newState.sleepFor = t.maxSlack
		}
		if newState.sleepFor > 0 {
			newState.last = newState.last.Add(newState.sleepFor)
		}
		taken = atomic.CompareAndSwapPointer(&t.state, previousStatePointer, unsafe.Pointer(&newState))
	}
	t.clock.Sleep(newState.sleepFor)
	return newState.last
}

type unlimited struct{}

// NewUnlimited returns a RateLimiter that is not limited.
func NewUnlimited() Limiter {
	return unlimited{}
}

func (unlimited) Take() time.Time {
	return time.Now()
}
