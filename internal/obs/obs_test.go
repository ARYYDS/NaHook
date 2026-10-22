package obs

import (
	"bytes"
	"encoding/json"
	"strings"
	"sync"
	"testing"
)

// designKeys are the nine keys of design §6.7, spelled out so a rename in the
// package shows up here.
var designKeys = []string{
	"intercepted", "rate_limited", "retries", "released", "gave_up",
	"final_429", "passthrough", "bad_gateway", "client_gone",
}

func TestSnapshotListsEveryCounter(t *testing.T) {
	snap := NewStats().Snapshot()
	if len(snap) != len(designKeys) {
		t.Fatalf("snapshot has %d keys, want %d", len(snap), len(designKeys))
	}
	for _, key := range designKeys {
		value, ok := snap[key]
		if !ok {
			t.Fatalf("snapshot is missing %q: %v", key, snap)
		}
		if value != 0 {
			t.Fatalf("%q = %d, want 0 on a fresh Stats", key, value)
		}
	}
}

func TestInc(t *testing.T) {
	stats := NewStats()
	stats.Inc(Intercepted)
	stats.Inc(Intercepted)
	stats.Inc(ClientGone)

	snap := stats.Snapshot()
	if snap["intercepted"] != 2 || snap["client_gone"] != 1 {
		t.Fatalf("snapshot = %v", snap)
	}
	for _, key := range designKeys {
		if key == "intercepted" || key == "client_gone" {
			continue
		}
		if snap[key] != 0 {
			t.Fatalf("%q = %d, want 0", key, snap[key])
		}
	}
}

func TestIncConcurrent(t *testing.T) {
	stats := NewStats()

	const (
		goroutines = 8
		each       = 1000
	)
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < each; j++ {
				stats.Inc(Retries)
				if i == 0 {
					stats.Inc(GaveUp)
				}
			}
		}(i)
	}
	wg.Wait()

	snap := stats.Snapshot()
	if want := int64(goroutines * each); snap["retries"] != want {
		t.Fatalf("retries = %d, want %d", snap["retries"], want)
	}
	if want := int64(each); snap["gave_up"] != want {
		t.Fatalf("gave_up = %d, want %d", snap["gave_up"], want)
	}
}

func TestCounterNamesMatchConstants(t *testing.T) {
	if len(counterNames) != numCounters {
		t.Fatalf("counterNames has %d entries, numCounters is %d", len(counterNames), numCounters)
	}
	seen := make(map[string]bool, numCounters)
	for c := Counter(0); c < numCounters; c++ {
		name := counterNames[c]
		if name == "" {
			t.Fatalf("counter %d has no name", c)
		}
		if seen[name] {
			t.Fatalf("duplicate counter name %q", name)
		}
		seen[name] = true
	}
}

func TestNewLoggerWritesJSONLines(t *testing.T) {
	var buf bytes.Buffer
	logger := NewLogger(&buf)

	logger.Info("config.reloaded", "uid", "0")

	line := strings.TrimSuffix(buf.String(), "\n")
	if strings.Contains(line, "\n") {
		t.Fatalf("logger wrote more than one line: %q", buf.String())
	}
	var event map[string]any
	if err := json.Unmarshal([]byte(line), &event); err != nil {
		t.Fatalf("log line is not JSON: %v (%q)", err, line)
	}
	if event["msg"] != "config.reloaded" {
		t.Fatalf("msg = %v", event["msg"])
	}
	if event["uid"] != "0" {
		t.Fatalf("uid = %v", event["uid"])
	}
	if event["level"] != "INFO" {
		t.Fatalf("level = %v", event["level"])
	}
}
