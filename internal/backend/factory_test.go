package backend

import "testing"

// TestHasIdleWatchdog pins the per-backend idle-watchdog contract that
// engine auto-retry policy depends on (a wall-timeout from a watchdog-less
// backend is retryable-with-rotation; from a watchdog-equipped backend it is
// terminal). antigravity is the lone backend with no idle watchdog because
// `agy -p` emits no incremental stdout — see newRaw's antigravity case.
//
// The table is cross-checked against Names() below so that adding a backend
// to the Catalog without classifying its watchdog here fails loudly instead
// of silently defaulting to "has a watchdog".
func TestHasIdleWatchdog(t *testing.T) {
	want := map[string]bool{
		"claude":      true,
		"gemini":      true,
		"codex":       true,
		"opencode":    true,
		"antigravity": false, // the wedge-prone exception this guards
	}

	for _, name := range Names() {
		exp, ok := want[name]
		if !ok {
			t.Fatalf("backend %q is in the Catalog but not classified in this test — "+
				"add it to `want` AND confirm newRaw arms (or skips) its idle watchdog, "+
				"because engine auto-retry keys wall-timeout recovery on HasIdleWatchdog", name)
		}
		if got := HasIdleWatchdog(name); got != exp {
			t.Errorf("HasIdleWatchdog(%q) = %v; want %v (MUST match newRaw's watchdog wiring)", name, got, exp)
		}
	}

	// Unknown / unattributed names are conservatively treated as having a
	// watchdog so an unattributed wall-timeout is never auto-retried.
	if !HasIdleWatchdog("") {
		t.Error(`HasIdleWatchdog("") = false; want true (unattributed wall-timeout must stay terminal)`)
	}
	if !HasIdleWatchdog("does-not-exist") {
		t.Error("HasIdleWatchdog(unknown) = false; want true (conservative default)")
	}
}
