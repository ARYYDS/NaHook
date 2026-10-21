package gate

import (
	"regexp"
	"slices"
	"strings"
	"sync"
)

// orgRe pulls the upstream organization out of the rate-limit text
// (渠道监控/429-vs-quota.md: "in organization org-…").
var orgRe = regexp.MustCompile(`in organization (org-[A-Za-z0-9]+)`)

// Key builds the gate key of one interception: the model plus the upstream
// organization, or "-" when the message names none (design §1.5).
func Key(model, message string) string {
	if m := orgRe.FindStringSubmatch(message); m != nil {
		return model + "|" + m[1]
	}
	return model + "|-"
}

// Registry holds one Gate per key (design §6.3). There are only models × orgs
// of them, so nothing is ever evicted.
type Registry struct {
	m sync.Map // map[string]*Gate
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{}
}

// Get returns the gate of key, creating it on first use.
func (r *Registry) Get(key string) *Gate {
	v, _ := r.m.LoadOrStore(key, new(Gate))
	return v.(*Gate)
}

// ResetModel clears the streak of every gate of one model. A released stream
// does not know which org it would have been keyed by, so all keys of the model
// are reset (design §6.3).
func (r *Registry) ResetModel(model string) {
	prefix := model + "|"
	r.m.Range(func(key, value any) bool {
		if strings.HasPrefix(key.(string), prefix) {
			value.(*Gate).OnReleased()
		}
		return true
	})
}

// Snapshot returns the current gate states for GET /v1/stats, ordered by key so
// the response is stable between calls.
func (r *Registry) Snapshot() []GateStat {
	out := make([]GateStat, 0, 8)
	r.m.Range(func(key, value any) bool {
		out = append(out, value.(*Gate).stat(key.(string)))
		return true
	})
	slices.SortFunc(out, func(a, b GateStat) int { return strings.Compare(a.Key, b.Key) })
	return out
}
