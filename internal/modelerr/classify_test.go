package modelerr

// Classifier Tests
//
// Governing tests: SPEC-0013 REQ-3; design.md "Testing" — a classifier table
// per adapter, plus the unclassified control. The notes are the shapes agents
// actually record: crush's finish part ("<status text>: <provider body>"),
// Claude Code's own summaries, codex's stream and retry errors, and the raw
// Anthropic/OpenAI/litellm bodies any of them can relay.
//
// @joestump-agent 09/21/2026 - Added for harness#356.
//
// @joestump 10/04/2026 - Moved from internal/metrics with the classifier
// (harness#473). The unclassified control's end-to-end test drives the
// metrics pipeline, so it stayed there (internal/metrics/unclassified_test.go).

import (
	"testing"
)

type classCase struct {
	note  string
	class Class
	known bool
}

func runClassTable(t *testing.T, adapter string, cases []classCase) {
	t.Helper()
	for _, c := range cases {
		class, known := Classify(adapter, c.note)
		if class != c.class || known != c.known {
			t.Errorf("Classify(%q, %q) = (%s, %v), want (%s, %v)", adapter, c.note, class, known, c.class, c.known)
		}
	}
}

func TestClassifyCrush(t *testing.T) {
	runClassTable(t, "crush", []classCase{
		// The 2026-09-14 shape: a weekly quota, relayed through crush.
		{`Too Many Requests: {"type":"error","error":{"type":"rate_limit_error","message":"This request would exceed your account's rate limit."}}`, ClassQuota, true},
		{"Too Many Requests: You have reached your weekly limit. It resets Monday 00:00 UTC.", ClassQuota, true},
		{"429 Too Many Requests", ClassQuota, true},
		{`Bad Request: litellm.RateLimitError: AnthropicException - {"type":"error"}`, ClassQuota, true},
		{"litellm.BudgetExceededError: Budget has been exceeded! Current cost: 50.1, Max budget: 50.0", ClassQuota, true},
		{"You exceeded your current quota, please check your plan and billing details. insufficient_quota", ClassQuota, true},
		{"Payment Required: 402", ClassQuota, true},
		{"Bad Request: Your credit balance is too low to access the Anthropic API.", ClassQuota, true},
		{"Unauthorized: invalid x-api-key", ClassAuth, true},
		{`litellm.AuthenticationError: {"type":"authentication_error","message":"invalid api key"}`, ClassAuth, true},
		{`Forbidden: {"type":"error","error":{"type":"permission_error"}}`, ClassAuth, true},
		{"litellm.Timeout: Request timed out after 600.0 seconds", ClassTimeout, true},
		{"Post \"https://api.anthropic.com/v1/messages\": context deadline exceeded", ClassTimeout, true},
		{"Gateway Timeout: 504", ClassTimeout, true},
		// A timeout while connecting is "could not be reached", not "accepted,
		// then too slow" (review, harness#589).
		{`Post "https://api.anthropic.com/v1/messages": dial tcp 160.79.104.10:443: i/o timeout`, ClassTransport, true},
		{"dial tcp 160.79.104.10:443: connect: connection timed out", ClassTransport, true},
		{`Post "https://litellm.example/v1/chat/completions": net/http: TLS handshake timeout`, ClassTransport, true},
		{"Service Unavailable: upstream connect error or disconnect/reset before headers. reset reason: connection timeout", ClassTransport, true},
		{"Post \"https://api.anthropic.com/v1/messages\": context deadline exceeded (Client.Timeout exceeded while awaiting headers)", ClassTimeout, true},
		{"read tcp 10.0.0.2:51514->160.79.104.10:443: i/o timeout", ClassTimeout, true},
		{"litellm.APIConnectionError: Connection error.", ClassTransport, true},
		{`Post "https://api.anthropic.com/v1/messages": dial tcp: lookup api.anthropic.com: no such host`, ClassTransport, true},
		{"read tcp 10.0.0.2:51514->160.79.104.10:443: read: connection reset by peer", ClassTransport, true},
		{"unexpected EOF", ClassTransport, true},
		{"tls: failed to verify certificate: x509: certificate signed by unknown authority", ClassTransport, true},
		{"Bad Gateway: 502", ClassTransport, true},
		{"Service Unavailable: 503", ClassTransport, true},
		{`{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`, ClassTransport, true},
		{"529 Overloaded", ClassTransport, true},
		// Context window: recognised, class other, NOT unclassified.
		{"Bad Request: litellm.ContextWindowExceededError: prompt contains at least 196609 input tokens", ClassOther, true},
		{"Bad Request: prompt is too long: 213000 tokens > 200000 maximum", ClassOther, true},
		// The control: nothing matches.
		{"agent turn finished with an error", ClassOther, false},
		{"Bad Request: tool_use ids must be unique", ClassOther, false},
		// Word boundaries: a token count is not a status code.
		{"Bad Request: max_tokens 4290 exceeds the model limit of 4096 for this route", ClassOther, false},
	})
}

func TestClassifyClaudeCode(t *testing.T) {
	runClassTable(t, "claude-code", []classCase{
		{"Claude AI usage limit reached|1789430400", ClassQuota, true},
		{"Credit balance is too low", ClassQuota, true},
		{`API Error: 429 {"type":"error","error":{"type":"rate_limit_error"}}`, ClassQuota, true},
		{"Invalid API key · Please run /login", ClassAuth, true},
		{"OAuth token has expired. Please obtain a new token or refresh your existing token.", ClassAuth, true},
		{`API Error: 401 {"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`, ClassAuth, true},
		{"Request timed out.", ClassTimeout, true},
		{`API Error: 529 {"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`, ClassTransport, true},
		{`API Error: 500 {"type":"error","error":{"type":"api_error","message":"Internal server error"}}`, ClassTransport, true},
		{"API Error: Connection error.", ClassTransport, true},
		{"Prompt is too long", ClassOther, true},
		{"Something unexpected happened", ClassOther, false},
		// agent-trace's error-mark notes (stump.wtf/agent-trace#104):
		// "<code> (<status>): <text>", the status absent when no response came.
		{"rate_limit (429): You've hit your session limit · resets 3pm", ClassQuota, true},
		{"rate_limit (429): API Error: Server is temporarily limiting requests (not your usage limit)", ClassTransport, true},
		{"authentication_failed (401): Failed to authenticate. API Error: 401 Invalid authentication credentials", ClassAuth, true},
		{"authentication_failed: Failed to authenticate", ClassAuth, true},
		{"server_error (529): API Error: Repeated 529 Overloaded errors", ClassTransport, true},
		{"server_error: API Error: Unable to connect to API (ConnectionRefused)", ClassTransport, true},
		{"server_error: API Error: Can't reach the API", ClassTransport, true},
		{"server_error (521): API Error: 521 Web server is down", ClassTransport, true},
		{"server_error (504): API Error: upstream took too long", ClassTimeout, true},
		{"unknown: API Error: Overloaded", ClassTransport, true},
	})
}

func TestClassifyCodex(t *testing.T) {
	runClassTable(t, "codex", []classCase{
		{"You've hit your usage limit. Upgrade to Pro or try again in 3 days 4 hours.", ClassQuota, true},
		{"exceeded retry limit, last status: 429 Too Many Requests", ClassQuota, true},
		{"unexpected status 401 Unauthorized: Missing bearer or basic authentication in header", ClassAuth, true},
		{"stream disconnected before completion: stream closed before response.completed", ClassTransport, true},
		{"error sending request for url (https://api.openai.com/v1/responses)", ClassTransport, true},
		{"Request timed out", ClassTimeout, true},
		{"This model's maximum context length is 272000 tokens.", ClassOther, true},
		{"unexpected status 400 Bad Request: invalid schema for function", ClassOther, false},
	})
}

// An adapter's own phrasings are its own: the same note under an adapter that
// never prints it falls through to the shared table, and there it may match
// nothing — which the unclassified control then reports.
func TestClassifyAdapterTablesAreScoped(t *testing.T) {
	if c, known := Classify("claude-code", "Please run /login"); c != ClassAuth || !known {
		t.Errorf("claude-code login prompt = (%s, %v), want auth", c, known)
	}
	if c, known := Classify("generic", "Please run /login"); c != ClassOther || known {
		t.Errorf("login prompt under generic = (%s, %v), want unclassified other", c, known)
	}
	if c, known := Classify("codex", "stream disconnected before completion"); c != ClassTransport || !known {
		t.Errorf("codex stream disconnect = (%s, %v), want transport", c, known)
	}
	if c, known := Classify("crush", "stream disconnected before completion"); c != ClassOther || known {
		t.Errorf("codex phrasing under crush = (%s, %v), want unclassified other", c, known)
	}
}
