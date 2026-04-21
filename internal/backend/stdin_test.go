package backend

import (
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestWireNullStdinOpensDevNull verifies the helper explicitly sets cmd.Stdin
// to /dev/null when none is provided. This matters because the claude CLI has
// a startup stdin probe (~3s wait) that stalls indefinitely if stdin is an
// ambiguous inherited fd under systemd rather than a true null reader.
func TestWireNullStdinOpensDevNull(t *testing.T) {
	cmd := exec.Command("/bin/true")
	if cmd.Stdin != nil {
		t.Fatal("exec.Command should default to nil Stdin")
	}

	cleanup := wireNullStdin(cmd)
	defer cleanup()

	if cmd.Stdin == nil {
		t.Fatal("wireNullStdin must set cmd.Stdin to /dev/null, got nil")
	}

	// Read should return EOF immediately — that's what the claude CLI's
	// stdin probe needs to see to stop waiting.
	buf := make([]byte, 16)
	n, err := cmd.Stdin.Read(buf)
	if n != 0 || err != io.EOF {
		t.Errorf("want (0, io.EOF) from /dev/null read, got (%d, %v)", n, err)
	}
}

// TestWireNullStdinPreservesExistingStdin verifies that when a stdin prompt
// is already wired (large-prompt-via-stdin path), the helper leaves it alone.
// Regression: a bug where the helper overwrites prompt stdin would silently
// break the large-prompt path that every real workflow hits.
func TestWireNullStdinPreservesExistingStdin(t *testing.T) {
	cmd := exec.Command("/bin/true")
	cmd.Stdin = strings.NewReader("important prompt content")

	cleanup := wireNullStdin(cmd)
	defer cleanup()

	// Read should yield the prompt, not EOF.
	buf := make([]byte, 32)
	n, err := cmd.Stdin.Read(buf)
	if err != nil {
		t.Fatalf("read existing stdin: %v", err)
	}
	got := string(buf[:n])
	if got != "important prompt content" {
		t.Errorf("wireNullStdin clobbered existing stdin: got %q", got)
	}
}

// TestWireNullStdinCleanupClosesFile verifies the returned cleanup func
// actually closes the /dev/null file descriptor. A leak here would show up
// as fd exhaustion under sustained workflow load.
func TestWireNullStdinCleanupClosesFile(t *testing.T) {
	cmd := exec.Command("/bin/true")
	cleanup := wireNullStdin(cmd)

	f, ok := cmd.Stdin.(*os.File)
	if !ok {
		t.Fatalf("want *os.File, got %T", cmd.Stdin)
	}

	cleanup()

	// After close, further reads must fail. os.File returns file already
	// closed errors on Read after Close.
	if _, err := f.Read(make([]byte, 1)); err == nil {
		t.Error("file should be closed after cleanup func")
	}
}
