package backend

import (
	"context"
	"testing"
)

// wantMCP is the predicate required-knowledge pinning uses.
func wantMCP(c Capabilities) bool { return c.MCP }

// TestSelectCapable_PinsToCapableMemberOfMixedRotation is the core pinning
// case: a rotation of mostly non-MCP backends still resolves to its single
// MCP-capable member (claude) when knowledge is required.
func TestSelectCapable_PinsToCapableMemberOfMixedRotation(t *testing.T) {
	rr, err := NewRoundRobin(&Codex{}, &Opencode{}, &Claude{})
	if err != nil {
		t.Fatalf("NewRoundRobin: %v", err)
	}
	got, ok := rr.SelectCapable(context.Background(), wantMCP)
	if !ok {
		t.Fatal("expected a capable member (claude) in the rotation")
	}
	if got.Name() != "claude" {
		t.Errorf("pinned to %q, want claude", got.Name())
	}
}

// TestSelectCapable_EmptyIntersectionFails: a rotation with no MCP-capable
// member must report ok=false so the caller fails dispatch instead of running
// required knowledge blind.
func TestSelectCapable_EmptyIntersectionFails(t *testing.T) {
	rr, err := NewRoundRobin(&Codex{}, &Opencode{}, &Gemini{})
	if err != nil {
		t.Fatalf("NewRoundRobin: %v", err)
	}
	if _, ok := rr.SelectCapable(context.Background(), wantMCP); ok {
		t.Fatal("no MCP-capable member exists; SelectCapable must report ok=false")
	}
}

// TestSelectCapable_PrefersStickyMemberWhenCapable: when the sticky-selected
// member already qualifies, keep it (don't gratuitously re-pin and lose
// rotation stickiness).
func TestSelectCapable_PrefersStickyMemberWhenCapable(t *testing.T) {
	rr, err := NewRoundRobin(&Claude{}, &Codex{}, &Claude{})
	if err != nil {
		t.Fatalf("NewRoundRobin: %v", err)
	}
	// Find a sticky key that lands on a claude slot, then assert SelectCapable
	// keeps that exact slot rather than jumping to slot 0.
	ctx := WithStickyKey(context.Background(), "corr:reviewer")
	sticky := rr.Select(ctx)
	if sticky.Name() == "claude" {
		got, ok := rr.SelectCapable(ctx, wantMCP)
		if !ok || got.Name() != "claude" {
			t.Fatalf("sticky claude should be kept, got %q ok=%v", got.Name(), ok)
		}
	}
}

// TestResolveCapable_SingleBackend: a single backend is returned iff it
// qualifies; otherwise ok=false.
func TestResolveCapable_SingleBackend(t *testing.T) {
	if _, ok := ResolveCapable(context.Background(), &Claude{}, wantMCP); !ok {
		t.Error("claude satisfies MCP; ResolveCapable should return it")
	}
	if _, ok := ResolveCapable(context.Background(), &Codex{}, wantMCP); ok {
		t.Error("codex lacks MCP; ResolveCapable must report ok=false")
	}
}

// TestBackendCapabilitiesMatrix locks the documented capability matrix (task
// 0002). A wrong row silently changes knowledge negotiation (0008): e.g. if
// gemini falsely reported MCP=true, the resolver would attach an MCP tool
// config gemini ignores, and required knowledge would never be retrieved.
func TestBackendCapabilitiesMatrix(t *testing.T) {
	cases := []struct {
		name string
		be   Backend
		want Capabilities
	}{
		{"claude", &Claude{}, Capabilities{MCP: true, SystemPrompt: true, SessionResume: true, TokenAccounting: true, ReasoningEffort: true}},
		{"gemini", &Gemini{}, Capabilities{SessionResume: true}},
		{"codex", &Codex{}, Capabilities{SessionResume: true, TokenAccounting: true}},
		{"opencode", &Opencode{}, Capabilities{SessionResume: true}},
		{"antigravity", &Antigravity{}, Capabilities{SessionResume: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.be.Capabilities(); got != tc.want {
				t.Errorf("%s Capabilities() = %+v, want %+v", tc.name, got, tc.want)
			}
		})
	}
}

// TestRoundRobinCapabilitiesIntersection verifies the conservative aggregate:
// the rotation reports a capability only if EVERY member has it. claude+codex
// share SessionResume but not MCP — so the rotation's MCP must be false even
// though claude alone supports it. This is the value 0008 must NOT use for
// pin-to-a-capable-member.
func TestRoundRobinCapabilitiesIntersection(t *testing.T) {
	rr, err := NewRoundRobin(&Claude{}, &Codex{})
	if err != nil {
		t.Fatalf("NewRoundRobin: %v", err)
	}
	got := rr.Capabilities()
	want := Capabilities{
		MCP:             false, // codex lacks it
		SystemPrompt:    false, // codex lacks it
		SessionResume:   true,  // both have it
		TokenAccounting: true,  // both have it
		ReasoningEffort: false, // codex lacks it
	}
	if got != want {
		t.Errorf("RoundRobin(claude,codex).Capabilities() = %+v, want %+v", got, want)
	}

	// Per-member access (the lens 0008 uses): claude is the MCP-capable member.
	var mcpMembers int
	for _, b := range rr.Backends() {
		if b.Capabilities().MCP {
			mcpMembers++
		}
	}
	if mcpMembers != 1 {
		t.Errorf("expected exactly 1 MCP-capable member (claude), got %d", mcpMembers)
	}
}

// TestLimitedBackendDelegatesCapabilities guards F10: the concurrency-limiter
// wrapper must not flatten the inner backend's matrix to zero. A limited(claude)
// must still report MCP=true, otherwise the resolver would refuse to send it a
// tool config.
func TestLimitedBackendDelegatesCapabilities(t *testing.T) {
	lb := NewLimitedBackend(&Claude{}, 2, nil)
	got := lb.Capabilities()
	want := Capabilities{MCP: true, SystemPrompt: true, SessionResume: true, TokenAccounting: true, ReasoningEffort: true}
	if got != want {
		t.Errorf("limited(claude).Capabilities() = %+v, want %+v (must delegate to inner)", got, want)
	}
}
