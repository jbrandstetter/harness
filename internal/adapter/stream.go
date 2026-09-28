package adapter

// Structured Prompt Streams
//
// Some agent CLIs write a machine-readable event stream on stdout when they
// run a prompt one-shot. Claude Code's `-p --output-format stream-json` writes
// one JSON object per line: an init line, every message with its tool calls
// and results, and a final result line with the outcome, turns and cost.
//
// A one-shot like that has no screen for a terminal emulator to keep. Under a
// PTY, stderr is merged into the stream, line endings are translated, and two
// emulators render JSON nobody reads as a screen. So a one-shot whose adapter
// declares a structured stream is spawned on pipes instead: stdout carries
// exactly the stream and stderr exactly the diagnostics (internal/supervisor,
// pipes.go).
//
// Each adapter declares its own format, because only the adapter knows what
// its PromptCommand asks the CLI to print. An adapter that declares nothing
// keeps its PTY, and so does every resident harness and every `command`
// harness, whatever its argv.
//
// Governing: ADR-0033 "Normalization belongs to agent-trace" (an adapter
// declares whether its prompt command emits a structured stream and in which
// format) and "Structured one-shots run on pipes"; ADR-0011 (agent adapters);
// SPEC-0006 REQ "Structured Prompt Stream".
//
// @joestump-agent 09/28/2026 - Added: stream-json one-shots stop running under
// a PTY (https://github.com/stump-wtf/harness/issues/18).

// StreamFormat names the line-delimited format a prompt one-shot writes on
// stdout. The empty format means the CLI prints for a terminal.
type StreamFormat string

// StreamJSON is Claude Code's `--output-format stream-json`: one JSON object
// per line.
const StreamJSON StreamFormat = "stream-json"

// StructuredStreamer is the optional Adapter extension for an adapter whose
// PromptCommand makes the CLI write a structured stream on stdout. It is
// optional, like PeekFormatterProvider, so an adapter that says nothing keeps
// its PTY by construction.
type StructuredStreamer interface {
	// PromptStream is the format PromptCommand's argv asks the CLI to write.
	PromptStream() StreamFormat
}

// PromptStreamOf is a's declared prompt stream format, or "" when a declares
// none.
func PromptStreamOf(a Adapter) StreamFormat {
	if s, ok := a.(StructuredStreamer); ok {
		return s.PromptStream()
	}
	return ""
}

// PromptStream declares stream-json: PromptCommand always passes
// `--verbose --output-format stream-json`.
func (a *ClaudeCode) PromptStream() StreamFormat { return StreamJSON }
