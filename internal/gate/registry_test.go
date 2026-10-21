package gate

import (
	"sync"
	"testing"
	"time"
)

// ltMessage is the LT rate-limit text (渠道监控/429-vs-quota.md, 4-4-tpm-retry.md §2).
const ltMessage = "Rate limit reached for gpt-6-astra in organization org-9xKq2LmZ on tokens per min (TPM): " +
	"Limit 40000000, Used 40000000, Requested 19224. Please try again in 28ms."

func TestKey(t *testing.T) {
	tests := []struct {
		name    string
		model   string
		message string
		want    string
	}{
		{name: "LT message", model: "gpt-6-astra", message: ltMessage, want: "gpt-6-astra|org-9xKq2LmZ"},
		{
			name:    "RPM text of the other model",
			model:   "gpt-6-lunna",
			message: "Rate limit reached for gpt-6-lunna in organization org-Abc123 on requests per min (RPM): Limit 500",
			want:    "gpt-6-lunna|org-Abc123",
		},
		{name: "no organization", model: "gpt-6-astra", message: "Rate limit reached for gpt-6-astra", want: "gpt-6-astra|-"},
		{name: "empty message", model: "gpt-6-astra", message: "", want: "gpt-6-astra|-"},
		{name: "org prefix without an id", model: "gpt-6-astra", message: "in organization org-", want: "gpt-6-astra|-"},
		{name: "id ends at the first non alnum", model: "gpt-6-astra", message: "in organization org-x on tokens", want: "gpt-6-astra|org-x"},
		{name: "match is case sensitive", model: "gpt-6-astra", message: "IN ORGANIZATION org-abc", want: "gpt-6-astra|-"},
		{name: "model may be anything", model: "", message: ltMessage, want: "|org-9xKq2LmZ"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Key(tt.model, tt.message); got != tt.want {
				t.Fatalf("Key(%q, %q) = %q, want %q", tt.model, tt.message, got, tt.want)
			}
		})
	}
}

func TestRegistryGetReturnsSameGate(t *testing.T) {
	r := NewRegistry()

	first := r.Get("gpt-6-astra|org-1")
	again := r.Get("gpt-6-astra|org-1")
	other := r.Get("gpt-6-astra|org-2")

	if first != again {
		t.Fatal("Get returned two different gates for the same key")
	}
	if first == other {
		t.Fatal("Get returned the same gate for two different keys")
	}

	first.OnRateLimit(10*time.Millisecond, 10*time.Millisecond, 3)
	if got := again.streak.Load(); got != 1 {
		t.Fatalf("state is not shared: streak = %d, want 1", got)
	}
	if got := other.streak.Load(); got != 0 {
		t.Fatalf("the other key was touched: streak = %d, want 0", got)
	}
}

func TestRegistryGetConcurrent(t *testing.T) {
	r := NewRegistry()

	const n = 64
	gates := make([]*Gate, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			gates[i] = r.Get("gpt-6-astra|org-1")
		}(i)
	}
	wg.Wait()

	for i := 1; i < n; i++ {
		if gates[i] != gates[0] {
			t.Fatalf("goroutine %d got a different gate", i)
		}
	}
}

func TestRegistryResetModel(t *testing.T) {
	r := NewRegistry()

	keys := []string{
		"gpt-6-astra|org-1",
		"gpt-6-astra|org-2",
		"gpt-6-astra|-",
		"gpt-6-lunna|org-1",
	}
	for _, key := range keys {
		for i := 0; i < 2; i++ {
			r.Get(key).OnRateLimit(10*time.Millisecond, 10*time.Millisecond, 3)
		}
	}

	r.ResetModel("gpt-6-astra")

	wantStreak := map[string]int32{
		"gpt-6-astra|org-1": 0,
		"gpt-6-astra|org-2": 0,
		"gpt-6-astra|-":     0,
		"gpt-6-lunna|org-1": 2,
	}
	for key, want := range wantStreak {
		if got := r.Get(key).streak.Load(); got != want {
			t.Fatalf("streak of %q = %d, want %d", key, got, want)
		}
	}

	// A prefix without the separator must not touch longer model names.
	r.ResetModel("gpt-6")
	if got := r.Get("gpt-6-astra|org-1").streak.Load(); got != 0 {
		t.Fatalf("ResetModel(gpt-6) changed gpt-6-astra state: streak = %d", got)
	}
	if got := r.Get("gpt-6-lunna|org-1").streak.Load(); got != 2 {
		t.Fatalf("ResetModel(gpt-6) changed gpt-6-lunna state: streak = %d", got)
	}

	// The model's own gates are still reset once more below.
	r.Get("gpt-6|org-9").OnRateLimit(10*time.Millisecond, 10*time.Millisecond, 3)
	r.ResetModel("gpt-6")
	if got := r.Get("gpt-6|org-9").streak.Load(); got != 0 {
		t.Fatalf("ResetModel(gpt-6) did not clear gpt-6|org-9: streak = %d", got)
	}
}

func TestRegistrySnapshot(t *testing.T) {
	r := NewRegistry()

	if got := r.Snapshot(); len(got) != 0 {
		t.Fatalf("empty registry snapshot = %+v, want empty", got)
	}

	for _, key := range []string{"c|-", "a|org-1", "b|org-2"} {
		r.Get(key).OnRateLimit(30*time.Millisecond, 20*time.Millisecond, 3)
	}

	got := r.Snapshot()
	wantOrder := []string{"a|org-1", "b|org-2", "c|-"}
	if len(got) != len(wantOrder) {
		t.Fatalf("snapshot = %+v, want %d rows", got, len(wantOrder))
	}
	now := time.Now().UnixMilli()
	for i, want := range wantOrder {
		if got[i].Key != want {
			t.Fatalf("snapshot row %d = %q, want %q (sorted by key)", i, got[i].Key, want)
		}
		if got[i].Streak != 1 {
			t.Fatalf("snapshot row %q: streak = %d, want 1", got[i].Key, got[i].Streak)
		}
		if got[i].BaseMS != 30 {
			t.Fatalf("snapshot row %q: base_ms = %d, want 30", got[i].Key, got[i].BaseMS)
		}
		if got[i].UntilMS < now-1 || got[i].UntilMS > now+30 {
			t.Fatalf("snapshot row %q: until_ms = %d, want about now+30ms (%d)", got[i].Key, got[i].UntilMS, now)
		}
	}
}
