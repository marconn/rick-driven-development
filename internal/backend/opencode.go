package backend

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"
)

// Opencode shells out to the opencode CLI (sst/opencode) in non-interactive
// `run` mode.
//
// Argv shape (verified against opencode v1.15.12 `opencode run --help`):
//   - `run`                          : non-interactive subcommand
//   - `--format json`                : NDJSON event stream on stdout (the
//     default "formatted" mode writes nothing to stdout — only TUI chrome to
//     stderr — so JSON is the only capturable mode)
//   - `-m provider/model`            : model override, ONLY when provider-
//     qualified (see model handling below)
//   - `-c` / `--continue`            : continue the most recent session
//   - `-s <id>` / `--session <id>`   : resume a specific session
//   - `--dangerously-skip-permissions` : auto-approve tool requests (yolo)
//   - `-- <prompt>`                  : the prompt, as a single positional arg
//     after a `--` terminator so a prompt beginning with `-` is never parsed
//     as a flag
//
// Output capture is a two-step hybrid because `opencode run --format json` has
// a stdout flush-on-dispose race (opencode v1.15.12): on ~1/3 of piped runs the
// assistant `text` event is generated on the internal bus but lost from stdout
// before the process exits (measured 4/6, then 4/4 with the workaround below).
// Additionally several model families (the gemini-flash class) never surface a
// `text` event to the JSON stream at all, and the process exits 0 even on a
// provider API error. So stdout is NOT the authoritative output source:
//
//  1. We run with `--format json` and parse the stream for three things that
//     ARE reliable: the `sessionID` (carried on the first event, step_start,
//     which always flushes), any `error` event (exit code is 0 on API errors,
//     so the event is the only failure signal we get from the run itself), and
//     a best-effort live `text` tee for streaming.
//  2. After a clean exit with no error event, we shell out to
//     `opencode export <sessionID>`, which reads opencode's persisted session
//     DB (not the racy stream) and reliably contains the full assistant
//     message. The last assistant message's text parts are the authoritative
//     Output. Export recovers text even for the flash-class models whose stream
//     never emitted a text event.
//
// Model handling: opencode's `-m` requires the `provider/model` form (e.g.
// `google/gemini-2.5-pro`). A bare name (`gemini-2.5-flash`) is rejected with
// "Model not found: gemini-2.5-flash/." before any work. Because a single
// global RICK_MODEL (often a bare name) flows into Request.Model for every
// backend in a mixed review rotation, forwarding a bare name would crash every
// opencode call and take it out of the rotation. So Request.Model is forwarded
// to `-m` ONLY when it contains a "/"; otherwise it is dropped and opencode
// uses its own configured default model. This mirrors the antigravity driver's
// "don't let RICK_MODEL break the rotation" stance.
//
// Unsupported:
//   - separate `--system-prompt` → opencode run has no such flag; the system
//     prompt is folded into the prompt body inside a <system_instructions>
//     wrapper, matching the Gemini / Antigravity drivers.
//   - prompt via stdin → opencode run does not read the prompt from stdin (a
//     piped stdin yields an empty prompt). The prompt is therefore always
//     passed as a positional argv element; there is no maxArgSize stdin
//     fallback. A prompt large enough to hit ARG_MAX surfaces as an exec
//     E2BIG error in the returned BackendError rather than silent truncation.
//   - structured token accounting → StopReason and TokensUsed stay empty;
//     the export payload does carry usage, but per-call token attribution is
//     left to future work (Gemini behaves the same).
//   - `--mcp-config` → MCPConfig on the Request is ignored.
type Opencode struct {
	binaryPath   string
	stallTimeout time.Duration // 0 = no idle watchdog (default)
}

// NewOpencode creates an Opencode backend. binaryPath is the path to the
// `opencode` CLI binary.
func NewOpencode(binaryPath string) *Opencode {
	return &Opencode{binaryPath: binaryPath}
}

func (o *Opencode) Name() string { return "opencode" }

// Capabilities: opencode resumes sessions (-c/--session). It has no
// system-prompt flag (folded into a <system_instructions> wrapper), no MCP tool
// retrieval surfaced here, leaves token usage empty, and has no
// reasoning-effort knob.
func (o *Opencode) Capabilities() Capabilities {
	return Capabilities{SessionResume: true}
}

func (o *Opencode) combinePrompt(systemPrompt, userPrompt string) string {
	return fmt.Sprintf("<system_instructions>\n%s\n</system_instructions>\n\n%s", systemPrompt, userPrompt)
}

// opencodeExportTimeout caps the post-run `opencode export` call. Export reads
// a local session DB, so it is fast; this is a safety bound, deliberately
// derived from a fresh context (not the run's, which may be near its deadline)
// so a long run does not starve the authoritative-output read.
const opencodeExportTimeout = 60 * time.Second

// buildArgs returns the argv for `opencode run`. Unlike the other drivers it
// returns no stdin prompt — opencode has no stdin prompt channel, so the
// prompt is always the trailing positional argument.
func (o *Opencode) buildArgs(req Request) []string {
	args := []string{"run", "--format", "json"}

	// Session handling: "" new, "latest" → --continue most recent, any other
	// non-empty value → resume that specific session id.
	switch req.SessionID {
	case "":
		// new session — no resume flag
	case "latest":
		args = append(args, "--continue")
	default:
		args = append(args, "--session", req.SessionID)
	}

	// Forward the model ONLY when provider-qualified — see type doc.
	if strings.Contains(req.Model, "/") {
		args = append(args, "-m", req.Model)
	}

	if req.Yolo {
		args = append(args, "--dangerously-skip-permissions")
	}

	// When resuming, the original session already carries the system prompt;
	// re-sending it would prepend a duplicate.
	var combined string
	if req.SessionID != "" {
		combined = req.UserPrompt
	} else {
		combined = o.combinePrompt(req.SystemPrompt, req.UserPrompt)
	}

	// `--` terminates flag parsing so a prompt that begins with `-` is treated
	// as the positional message, not an unknown flag.
	args = append(args, "--", combined)
	return args
}

// opencodeStreamEvent is the subset of an `opencode run --format json` NDJSON
// event we care about. Every event carries sessionID; text parts carry the
// assistant text; an error event carries the failure cause (the run still
// exits 0, so this is our only in-stream failure signal).
type opencodeStreamEvent struct {
	Type      string `json:"type"`
	SessionID string `json:"sessionID"`
	Part      struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"part"`
	Error *struct {
		Name string `json:"name"`
		Data struct {
			Message string `json:"message"`
		} `json:"data"`
	} `json:"error"`
}

// opencodeExport is the subset of `opencode export <sessionID>` output we read.
type opencodeExport struct {
	Messages []struct {
		Info struct {
			Role string `json:"role"`
		} `json:"info"`
		Parts []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"parts"`
	} `json:"messages"`
}

// opencodeStreamResult holds what we scrape from the run's stdout stream.
type opencodeStreamResult struct {
	sessionID string
	errMsg    string // non-empty when an error event was seen
	text      string // best-effort streamed text (may be partial/empty; race)
}

// parseStream scans the captured NDJSON stdout for the session id, an error
// event, and any streamed text parts. Malformed lines are skipped — the stream
// interleaves events we don't model.
func parseOpencodeStream(captured string) opencodeStreamResult {
	var res opencodeStreamResult
	var text strings.Builder
	sc := bufio.NewScanner(strings.NewReader(captured))
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var ev opencodeStreamEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			continue
		}
		if res.sessionID == "" && ev.SessionID != "" {
			res.sessionID = ev.SessionID
		}
		if ev.Type == "error" && ev.Error != nil && res.errMsg == "" {
			if msg := ev.Error.Data.Message; msg != "" {
				res.errMsg = msg
			} else {
				res.errMsg = ev.Error.Name
			}
		}
		if ev.Type == "text" && ev.Part.Type == "text" {
			text.WriteString(ev.Part.Text)
		}
	}
	res.text = text.String()
	return res
}

// export reads the authoritative assistant text for a session from opencode's
// persisted store. Returns the concatenated text parts of the LAST assistant
// message (the response produced by the run we just completed; on a resumed
// session, export returns the full history and only the latest assistant turn
// is ours). Reasoning parts are deliberately excluded — only `text` parts.
func (o *Opencode) export(ctx context.Context, sessionID string) (string, error) {
	exCtx, cancel := context.WithTimeout(ctx, opencodeExportTimeout)
	defer cancel()

	cmd := exec.CommandContext(exCtx, o.binaryPath, "export", sessionID)
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("opencode export %s: %w: %s", sessionID, err, strings.TrimSpace(errBuf.String()))
	}

	var exp opencodeExport
	if err := json.Unmarshal(out.Bytes(), &exp); err != nil {
		return "", fmt.Errorf("opencode export %s: parse: %w", sessionID, err)
	}

	var last string
	for _, m := range exp.Messages {
		if m.Info.Role != "assistant" {
			continue
		}
		var b strings.Builder
		for _, p := range m.Parts {
			if p.Type == "text" {
				b.WriteString(p.Text)
			}
		}
		// Keep the most recent assistant message that actually carried text.
		if t := b.String(); t != "" {
			last = t
		}
	}
	return last, nil
}

func (o *Opencode) Run(ctx context.Context, req Request) (*Response, error) {
	start := time.Now()
	args := o.buildArgs(req)

	watchCtx, progress, stopWatch := WithIdleTimeout(ctx, o.stallTimeout)
	defer stopWatch()

	cmd := exec.CommandContext(watchCtx, o.binaryPath, args...)
	cmd.Dir = req.WorkDir
	configureProcessGroup(cmd, defaultKillGraceDelay)

	// Capture raw NDJSON stdout for post-exit parsing (sessionID + error) and
	// idle-watchdog liveness. We do not tee raw JSON to req.Output — it would
	// be unreadable; the resolved text is written to req.Output at the end.
	var captured bytes.Buffer
	cmd.Stdout = newProgressWriter(&captured, progress)

	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf

	// opencode does not read the prompt from stdin — wire /dev/null so a
	// startup stdin probe never blocks.
	closeStdin := wireNullStdin(cmd)
	defer closeStdin()

	if err := cmd.Run(); err != nil {
		stderr := captureErrorTail(stderrBuf.String(), captured.String())
		elapsed := time.Since(start)
		if errors.Is(context.Cause(watchCtx), ErrIdleTimeout) {
			return nil, &BackendError{
				Backend:  "opencode",
				Inner:    fmt.Errorf("%w (stall=%s)", ErrIdleTimeout, o.stallTimeout),
				Duration: elapsed,
				Stderr:   stderr,
			}
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, &BackendError{
				Backend:  "opencode",
				Inner:    ctxErr,
				Duration: elapsed,
				Stderr:   stderr,
			}
		}
		return nil, &BackendError{
			Backend:  "opencode",
			Inner:    err,
			Duration: elapsed,
			Stderr:   stderr,
		}
	}

	stream := parseOpencodeStream(captured.String())

	// The process exits 0 even on a provider API error, so the in-stream error
	// event is the authoritative failure signal.
	if stream.errMsg != "" {
		return nil, &BackendError{
			Backend:  "opencode",
			Inner:    errors.New(stream.errMsg),
			Duration: time.Since(start),
			Stderr:   captureErrorTail(stderrBuf.String(), captured.String()),
		}
	}

	if stream.sessionID == "" {
		// step_start (which carries sessionID) reliably flushes first; an empty
		// id means we got no usable stream at all.
		return nil, &BackendError{
			Backend:  "opencode",
			Inner:    errors.New("no session id in output stream"),
			Duration: time.Since(start),
			Stderr:   captureErrorTail(stderrBuf.String(), captured.String()),
		}
	}

	// Authoritative output comes from the persisted session, not the racy
	// stream. Fall back to whatever the stream captured if export fails.
	output, exportErr := o.export(ctx, stream.sessionID)
	if exportErr != nil {
		if stream.text == "" {
			return nil, &BackendError{
				Backend:  "opencode",
				Inner:    exportErr,
				Duration: time.Since(start),
				Stderr:   captureErrorTail(stderrBuf.String(), captured.String()),
			}
		}
		output = stream.text
	} else if output == "" && stream.text != "" {
		// Export succeeded but found no assistant text; prefer any streamed text.
		output = stream.text
	}

	if req.Output != nil && output != "" {
		_, _ = io.WriteString(req.Output, output)
	}

	return &Response{
		Output:   output,
		Duration: time.Since(start),
	}, nil
}
