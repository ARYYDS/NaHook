// Package obs holds the process logger and the atomic counters (design §6.7,
// interface 2.7).
package obs

import (
	"io"
	"log/slog"
	"sync/atomic"
)

// NewLogger returns the JSON logger the process runs with. cmd/nahook passes
// os.Stdout and journald collects one event per line.
func NewLogger(w io.Writer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(w, nil))
}

// Counter identifies one counter of the data plane.
type Counter int

const (
	Intercepted Counter = iota // intercepted: classify decided to intercept
	RateLimited                // rate_limited: one attempt was judged a rate limit
	Retries                    // retries: decided to wait and try again
	Released                   // released: the intercepted stream was released
	GaveUp                     // gave_up: the stand-in 429 was returned
	Passthrough                // passthrough: forwarded without interception, or a non-200/non-SSE reply
	BadGateway                 // bad_gateway: NewAPI was unreachable, answered 502
	ClientGone                 // client_gone: the client's context ended the forward or the wait
)

// numCounters is how many counters exist. Snapshot always lists all of them.
const numCounters = 8

// counterNames is the GET /v1/stats key of each counter, in constant order.
var counterNames = [numCounters]string{
	Intercepted: "intercepted",
	RateLimited: "rate_limited",
	Retries:     "retries",
	Released:    "released",
	GaveUp:      "gave_up",
	Passthrough: "passthrough",
	BadGateway:  "bad_gateway",
	ClientGone:  "client_gone",
}

// Stats is one atomic counter per event. Inc takes no lock, so the data plane
// never contends; a restart starts every counter at zero.
type Stats struct {
	counters [numCounters]atomic.Int64
}

// NewStats returns counters that are all zero.
func NewStats() *Stats {
	return &Stats{}
}

// Inc adds one to c. c is always one of this package's constants, so an
// out-of-range value would be a programming error.
func (s *Stats) Inc(c Counter) {
	s.counters[c].Add(1)
}

// Snapshot returns every counter by name, including the ones still at zero.
func (s *Stats) Snapshot() map[string]int64 {
	out := make(map[string]int64, numCounters)
	for c := Counter(0); c < numCounters; c++ {
		out[counterNames[c]] = s.counters[c].Load()
	}
	return out
}
