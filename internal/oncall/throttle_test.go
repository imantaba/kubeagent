package oncall

import (
	"testing"
	"time"
)

var t0 = time.Date(2026, 7, 26, 10, 0, 0, 0, time.UTC)

func TestCooldownBlocksARepeatAndReleasesAfterTheWindow(t *testing.T) {
	th := NewThrottle(time.Hour, 100)
	if !th.Allow("a", t0) {
		t.Fatal("first call must be allowed")
	}
	if th.Allow("a", t0.Add(59*time.Minute)) {
		t.Error("a repeat inside the cooldown must be blocked")
	}
	if !th.Allow("a", t0.Add(61*time.Minute)) {
		t.Error("a repeat past the cooldown must be allowed")
	}
}

func TestZeroCooldownAllowsImmediateRepeats(t *testing.T) {
	th := NewThrottle(0, 100)
	if !th.Allow("a", t0) || !th.Allow("a", t0) {
		t.Error("a zero cooldown must not block repeats")
	}
}

func TestBudgetAllowsABurstThenDenies(t *testing.T) {
	th := NewThrottle(0, 3)
	for i := 0; i < 3; i++ {
		if !th.Allow("a", t0) {
			t.Fatalf("call %d must be inside the burst capacity", i+1)
		}
	}
	if th.Allow("a", t0) {
		t.Error("the fourth call must exhaust the bucket")
	}
}

func TestBudgetRefillsContinuously(t *testing.T) {
	th := NewThrottle(0, 20) // 20/hour = one token every 3 minutes
	for i := 0; i < 20; i++ {
		th.Allow("a", t0)
	}
	if th.Allow("a", t0.Add(2*time.Minute)) {
		t.Error("two minutes must not yet have refilled a whole token")
	}
	if !th.Allow("a", t0.Add(4*time.Minute)) {
		t.Error("four minutes must have refilled a token")
	}
}

// The check order is the property: cooldown is evaluated first and costs
// nothing, so a cooldown-blocked object cannot spend budget another object
// needs. With capacity 2, objects a and b must both get through even though a
// was asked for twice.
func TestCooldownBlockedCallDoesNotConsumeBudget(t *testing.T) {
	th := NewThrottle(time.Hour, 2)
	if !th.Allow("a", t0) {
		t.Fatal("a must be allowed")
	}
	if th.Allow("a", t0.Add(time.Minute)) {
		t.Fatal("a must be cooldown-blocked")
	}
	if !th.Allow("b", t0.Add(2*time.Minute)) {
		t.Error("b must still have budget: the blocked repeat must not have spent a token")
	}
	if th.Allow("c", t0.Add(3*time.Minute)) {
		t.Error("c must be budget-denied: capacity was 2 and two calls were allowed")
	}
}

// A budget-denied object was never explained, so it must not be stamped with a
// cooldown — it stays eligible the moment budget returns.
func TestBudgetDeniedObjectStaysEligible(t *testing.T) {
	th := NewThrottle(30*time.Minute, 1)
	if !th.Allow("a", t0) {
		t.Fatal("a must be allowed")
	}
	if th.Allow("b", t0) {
		t.Fatal("b must be budget-denied")
	}
	if !th.Allow("b", t0.Add(time.Hour)) {
		t.Error("b was never explained, so it must be eligible once the bucket refills")
	}
}

func TestCountersTrackAllowedAndThrottled(t *testing.T) {
	th := NewThrottle(time.Hour, 1)
	th.Allow("a", t0) // allowed
	th.Allow("a", t0) // throttled: cooldown
	th.Allow("b", t0) // throttled: budget
	allowed, throttled := th.Counters()
	if allowed != 1 || throttled != 2 {
		t.Errorf("counters = (%d, %d), want (1, 2)", allowed, throttled)
	}
}

func TestRemainingReportsProjectedTokensWithoutMutating(t *testing.T) {
	th := NewThrottle(0, 20)
	for i := 0; i < 20; i++ {
		th.Allow("a", t0)
	}
	if got := th.Remaining(t0); got != 0 {
		t.Errorf("Remaining right after exhaustion = %g, want 0", got)
	}
	if got := th.Remaining(t0.Add(30 * time.Minute)); got < 9.9 || got > 10.1 {
		t.Errorf("Remaining after 30m = %g, want about 10", got)
	}
	if got := th.Remaining(t0.Add(10 * time.Hour)); got != 20 {
		t.Errorf("Remaining must clamp to capacity, got %g", got)
	}
	// Remaining must not have refilled the bucket as a side effect.
	if got := th.Remaining(t0); got != 0 {
		t.Errorf("Remaining mutated the bucket: re-reading at t0 gave %g, want 0", got)
	}
}

// Only allowed calls are stamped, and stamps older than the cooldown are
// pruned, so the map cannot grow without bound in a long-lived daemon.
func TestStampMapIsPruned(t *testing.T) {
	th := NewThrottle(time.Hour, 100000)
	for i := 0; i < 500; i++ {
		th.Allow(string(rune('a'+i%26))+string(rune('a'+i/26)), t0.Add(time.Duration(i)*time.Second))
	}
	before := len(th.seen)
	th.Allow("zz", t0.Add(48*time.Hour))
	if len(th.seen) >= before {
		t.Errorf("stamps older than the cooldown must be pruned: %d before, %d after", before, len(th.seen))
	}
	if len(th.seen) != 1 {
		t.Errorf("every stamp predates the cooldown window, so only the newest must remain; got %d", len(th.seen))
	}
}
