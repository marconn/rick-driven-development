package backend

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestBackendErrorUnwrapsIdleTimeout(t *testing.T) {
	be := &BackendError{
		Backend:  "claude",
		Inner:    ErrIdleTimeout,
		Duration: 2 * time.Minute,
		Stderr:   "subprocess was silent",
	}

	if !errors.Is(be, ErrIdleTimeout) {
		t.Errorf("errors.Is(be, ErrIdleTimeout) = false; want true so the handler can classify this as idle_timeout")
	}
}

func TestBackendErrorUnwrapsContextDeadline(t *testing.T) {
	be := &BackendError{
		Backend:  "claude",
		Inner:    context.DeadlineExceeded,
		Duration: 20 * time.Minute,
	}

	if !errors.Is(be, context.DeadlineExceeded) {
		t.Errorf("errors.Is(be, DeadlineExceeded) = false; want true so the handler can classify this as wall_timeout")
	}
}

func TestBackendErrorExposesStderrViaErrorsAs(t *testing.T) {
	original := &BackendError{
		Backend:  "codex",
		Inner:    errors.New("exit status 1"),
		Duration: 3 * time.Second,
		Stderr:   "fatal: config missing",
	}

	// Wrap in an outer error as the AIHandler does: fmt.Errorf("handler x: backend: %w", err).
	wrapped := &simpleWrap{msg: "handler developer: backend", inner: original}

	var extracted *BackendError
	if !errors.As(wrapped, &extracted) {
		t.Fatalf("errors.As through wrapper failed; expected to recover BackendError with stderr")
	}
	if extracted.Stderr != "fatal: config missing" {
		t.Errorf("Stderr = %q; want %q", extracted.Stderr, "fatal: config missing")
	}
	if extracted.Backend != "codex" {
		t.Errorf("Backend = %q; want codex", extracted.Backend)
	}
}

func TestBackendErrorMessageIncludesBackendAndStderrSnippet(t *testing.T) {
	be := &BackendError{
		Backend:  "claude",
		Inner:    errors.New("signal: killed"),
		Duration: 5 * time.Second,
		Stderr:   "auth failure: bad token\nrefusing to start\n",
	}
	msg := be.Error()
	if !strings.HasPrefix(msg, "claude: ") {
		t.Errorf("message should start with backend prefix: %q", msg)
	}
	if !strings.Contains(msg, "auth failure") {
		t.Errorf("message should contain stderr snippet: %q", msg)
	}
}

func TestTailBytesTruncatesWithMarker(t *testing.T) {
	// Stderr longer than the cap should be truncated at the head with a marker,
	// so operators know they're seeing the tail and not the whole transcript.
	in := strings.Repeat("x", MaxStderrCapture*2)
	got := tailBytes(in, MaxStderrCapture)
	if len(got) != MaxStderrCapture {
		t.Fatalf("len = %d; want exactly %d (MaxStderrCapture)", len(got), MaxStderrCapture)
	}
	if !strings.HasPrefix(got, "...[truncated]...") {
		t.Errorf("truncated output should start with marker, got %q", got[:30])
	}
}

func TestTailBytesNoOpWhenUnderCap(t *testing.T) {
	in := "short stderr"
	got := tailBytes(in, MaxStderrCapture)
	if got != in {
		t.Errorf("tailBytes mutated under-cap input: got %q, want %q", got, in)
	}
}

// simpleWrap is a minimal test-only wrapper. We use it (instead of
// fmt.Errorf("%w")) to prove errors.As traverses any implementor of Unwrap().
type simpleWrap struct {
	msg   string
	inner error
}

func (s *simpleWrap) Error() string { return s.msg + ": " + s.inner.Error() }
func (s *simpleWrap) Unwrap() error { return s.inner }
