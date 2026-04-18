package buildinfo

import (
	"sync"
	"testing"
)

// TestVersion_NonEmpty verifies that Version() returns a non-empty string in
// all build contexts. In CI the binary may have no VCS stamp, so the function
// must return "unknown" rather than an empty string.
func TestVersion_NonEmpty(t *testing.T) {
	// Reset the once so we can call resolve() again from a clean state in
	// this test process — the once may have already fired from a prior call.
	// We test resolve() directly to sidestep the package-level singleton.
	got := resolve()
	if got == "" {
		t.Errorf("resolve() returned empty string; want non-empty (got %q)", got)
	}
	t.Logf("resolved version: %q", got)
}

// TestVersion_Idempotent verifies that concurrent callers always receive the
// same string — the cache must be race-free.
func TestVersion_Idempotent(t *testing.T) {
	var wg sync.WaitGroup
	results := make([]string, 20)
	for i := range results {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			results[idx] = Version()
		}(i)
	}
	wg.Wait()

	first := results[0]
	if first == "" {
		t.Fatal("Version() returned empty string")
	}
	for i, v := range results {
		if v != first {
			t.Errorf("results[%d]=%q differs from results[0]=%q — not idempotent", i, v, first)
		}
	}
}
