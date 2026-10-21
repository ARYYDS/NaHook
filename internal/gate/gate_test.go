package gate

import (
	"context"
	"errors"
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// approx fails when got is above want or more than slack below it. "Above" is
// exact: a deadline or a wait may never be longer than the configured window,
// only measurable latency may shorten what is left when the test looks.
func approx(t *testing.T, label string, got, want, slack time.Duration) {
	t.Helper()
	if got > want || got < want-slack {
		t.Fatalf("%s = %v, want about %v (slack %v)", label, got, want, slack)
	}
}

func TestOnRateLimitUsesFloor(t *testing.T) {
	g := &Gate{}
	g.OnRateLimit(5*time.Millisecond, 200*time.Millisecond, 3)

	if got := time.Duration(g.base.Load()); got != 200*time.Millisecond {
		t.Fatalf("base = %v, want the floor 200ms", got)
	}
	if got := g.streak.Load(); got != 1 {
		t.Fatalf("streak = %d, want 1", got)
	}
	approx(t, "remaining", time.Duration(g.until.Load()-time.Now().UnixNano()), 200*time.Millisecond, 50*time.Millisecond)
}

func TestOnRateLimitKeepsElapsedAboveFloor(t *testing.T) {
	g := &Gate{}
	g.OnRateLimit(700*time.Millisecond, 200*time.Millisecond, 3)

	if got := time.Duration(g.base.Load()); got != 700*time.Millisecond {
		t.Fatalf("base = %v, want the elapsed 700ms", got)
	}
	approx(t, "remaining", time.Duration(g.until.Load()-time.Now().UnixNano()), 700*time.Millisecond, 50*time.Millisecond)
}

func TestOnRateLimitWindowDoublesAndCaps(t *testing.T) {
	g := &Gate{}
	const base = 100 * time.Millisecond
	windows := []time.Duration{100 * time.Millisecond, 200 * time.Millisecond, 400 * time.Millisecond, 400 * time.Millisecond}

	for _, want := range windows {
		g.OnRateLimit(base, base, 3)

		// The window is what the cap applies to. The stored streak keeps
		// counting past the cap because design §1.5 caps only the value used
		// for the shift (`s := min(streak.Add(1), streakCap)`), so it is not
		// asserted here.
		approx(t, "remaining", time.Duration(g.until.Load()-time.Now().UnixNano()), want, 50*time.Millisecond)
	}
}

// TestUntilOnlyMovesForward is the T3 concurrency criterion: while writers keep
// calling OnRateLimit, a concurrent reader must never see the deadline go
// backwards.
func TestUntilOnlyMovesForward(t *testing.T) {
	g := &Gate{}

	const (
		writers   = 8
		callsEach = 250
	)
	var violations atomic.Int64

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		var seen atomic.Int64
		for {
			select {
			case <-stop:
				return
			default:
			}
			v := g.until.Load()
			for {
				prev := seen.Load()
				if v < prev {
					violations.Add(1)
					break
				}
				if v == prev || seen.CompareAndSwap(prev, v) {
					break
				}
			}
		}
	}()

	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < callsEach; i++ {
				g.OnRateLimit(time.Duration(w)*time.Millisecond, time.Duration(w+1)*time.Millisecond, 3)
			}
		}(w)
	}
	wg.Wait()
	close(stop)
	<-done

	if n := violations.Load(); n != 0 {
		t.Fatalf("until moved backwards %d times", n)
	}
	if g.until.Load() == 0 {
		t.Fatal("until was never set")
	}
}

// TestWaitFullJitter covers the three T3 criteria for Wait: 1000 concurrent
// waiters, none of them past until, and no relation between launch order and
// wait length. Wall-clock arrival gets a slack for wake-up latency; the timer
// itself is armed strictly inside the window.
func TestWaitFullJitter(t *testing.T) {
	g := &Gate{}
	const window = time.Second
	g.OnRateLimit(window, window, 3) // streak 1, so the window is one base
	until := g.until.Load()

	const (
		waiters   = 1000
		slack     = 150 * time.Millisecond
		maxLinear = 0.2
	)
	durs := make([]time.Duration, waiters)
	arrivals := make([]int64, waiters)

	var wg sync.WaitGroup
	for i := 0; i < waiters; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			start := time.Now()
			if err := g.Wait(context.Background()); err != nil {
				t.Errorf("Wait(%d) = %v, want nil", i, err)
				return
			}
			durs[i] = time.Since(start)
			arrivals[i] = time.Now().UnixNano()
		}(i)
	}
	wg.Wait()

	var sum time.Duration
	for i := 0; i < waiters; i++ {
		if durs[i] < 0 || durs[i] > window+slack {
			t.Fatalf("waiter %d waited %v, want [0, %v]", i, durs[i], window)
		}
		if late := time.Duration(arrivals[i] - until); late > slack {
			t.Fatalf("waiter %d arrived %v after until", i, late)
		}
		sum += durs[i]
	}

	if mean := sum / waiters; mean < window/3 || mean > window*2/3 {
		t.Fatalf("mean wait = %v, want about %v (uniform jitter, not a fixed sleep)", mean, window/2)
	}
	if r := correlation(durs); math.Abs(r) > maxLinear {
		t.Fatalf("wait length correlates with the launch order: r = %.3f", r)
	}
}

// correlation is the Pearson coefficient between the index and the value.
func correlation(values []time.Duration) float64 {
	n := float64(len(values))
	var sx, sy, sxy, sxx, syy float64
	for i, v := range values {
		x, y := float64(i), float64(v)
		sx += x
		sy += y
		sxy += x * y
		sxx += x * x
		syy += y * y
	}
	den := math.Sqrt((n*sxx - sx*sx) * (n*syy - sy*sy))
	if den == 0 {
		return 0
	}
	return (n*sxy - sx*sy) / den
}

func TestWaitReturnsImmediatelyWhenCool(t *testing.T) {
	g := &Gate{}

	start := time.Now()
	if err := g.Wait(context.Background()); err != nil {
		t.Fatalf("Wait = %v, want nil", err)
	}
	if d := time.Since(start); d > 100*time.Millisecond {
		t.Fatalf("Wait on a cool gate took %v", d)
	}

	// Nothing to wait for, so a done context is not an error either.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := g.Wait(ctx); err != nil {
		t.Fatalf("Wait on a cool gate with a cancelled ctx = %v, want nil", err)
	}
}

func TestWaitReturnsOnContextCancel(t *testing.T) {
	g := &Gate{}
	g.OnRateLimit(2*time.Second, 2*time.Second, 3)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	err := g.Wait(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait = %v, want context.Canceled", err)
	}
	if d := time.Since(start); d > 300*time.Millisecond {
		t.Fatalf("Wait returned after %v: the cancellation did not interrupt the timer", d)
	}
}

func TestWaitReturnsOnContextDeadline(t *testing.T) {
	g := &Gate{}
	g.OnRateLimit(2*time.Second, 2*time.Second, 3)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	start := time.Now()
	if err := g.Wait(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Wait = %v, want context.DeadlineExceeded", err)
	}
	if d := time.Since(start); d > 300*time.Millisecond {
		t.Fatalf("Wait returned after %v", d)
	}
}

func TestOnReleasedResetsStreak(t *testing.T) {
	g := &Gate{}
	for i := 0; i < 3; i++ {
		g.OnRateLimit(10*time.Millisecond, 10*time.Millisecond, 3)
	}
	if got := g.streak.Load(); got != 3 {
		t.Fatalf("streak = %d, want 3", got)
	}

	g.OnReleased()
	if got := g.streak.Load(); got != 0 {
		t.Fatalf("streak after release = %d, want 0", got)
	}

	g.OnRateLimit(10*time.Millisecond, 10*time.Millisecond, 3)
	if got := g.streak.Load(); got != 1 {
		t.Fatalf("streak after release and one interception = %d, want 1", got)
	}
}

func TestStatConvertsToMilliseconds(t *testing.T) {
	g := &Gate{}
	g.OnRateLimit(30*time.Millisecond, 20*time.Millisecond, 3)

	st := g.stat("m|org-1")
	if st.Key != "m|org-1" || st.Streak != 1 || st.BaseMS != 30 {
		t.Fatalf("stat = %+v, want key m|org-1, streak 1, base 30ms", st)
	}
	now := time.Now().UnixMilli()
	if st.UntilMS < now-1 || st.UntilMS > now+30 {
		t.Fatalf("until_ms = %d, want about now+30ms (%d): the window is the elapsed 30ms", st.UntilMS, now)
	}
}
