// Package modelerr reads a model error's note: ClassifyRule names its class
// and the rule that matched, and ResetAfter the reset time it carries. It is
// the one classifier: metrics counts its classes, and the budget park
// detector acts on them (SPEC-0021 REQ-11, REQ-12).
package modelerr

// Model Error Classifier
//
// An agent's error mark carries the provider's refusal as text. This maps that
// text to one of five classes where the error is observed, so an alert rule
// never has to regex provider wording (SPEC-0013 REQ-3). The classes are
// operational situations, not error types:
//
//   - quota: the account may not make calls right now — a rate limit, an
//     exhausted weekly or monthly allowance, a spent credit balance.
//     Restarting does not fix it, it has a reset time outside our control,
//     and it tends to hit every harness sharing the provider at once.
//   - auth: the credential is missing, wrong, expired, or not allowed. A
//     human has to fix configuration.
//   - timeout: the request was accepted and took too long.
//   - transport: the provider could not be reached, or could not serve the
//     request — refused and reset connections, DNS and TLS failures, 5xx
//     gateway errors, provider overload. Transient; nothing about our account
//     or configuration is wrong.
//   - other: everything else, including errors we recognise that fit none of
//     the above (a context-window rejection is the common one).
//
// Overload (Anthropic's 529 overloaded_error, a bare 503) is transport, not
// quota, on purpose. It is the provider's capacity, not our allowance: it has
// no reset time, it clears by itself, and filing it under quota would send an
// operator to check a budget that is fine. The alert that matters — quota
// climbing on every harness at once — must not fire for a busy afternoon.
//
// The tables are adapter-aware (design.md "Classifying a model error"): each
// adapter's own phrasings come first, then the provider shapes any adapter can
// relay, because crush, Claude Code and codex can all front the same Anthropic
// or OpenAI endpoint, directly or through litellm. Rule order is significant: a
// context-window rejection usually arrives as a 400 Bad Request, and a rate
// limit as "429 ... retry after a timeout", so the specific rule has to win.
//
// An error that matches nothing is class other AND unclassified. The
// unclassified counter is the classifier's control: when a provider rewords its
// errors, that counter rises instead of the classifier silently decaying into
// "everything is other" — which reads exactly like a healthy zero on the quota
// series.
//
// Governing: ADR-0020, SPEC-0013 REQ-3; design.md "Classifying a model error".
// ADR-0027, SPEC-0021 REQ-11 (one classifier for metrics and budgets).
//
// @joestump-agent 09/21/2026 - Added for harness#356.
//
// @joestump 10/04/2026 - Moved from internal/metrics for harness#473, so
// metrics and quota parking share one table (SPEC-0021 design.md "One
// classifier, moved to internal/modelerr"). The design.md above is SPEC-0013's.
//
// @joestump 10/04/2026 - Every rule has a name, and ClassifyRule returns it
// (harness#473): a park and doctor name the rule, never the error text. Rules
// that listed two situations were split; no note changes class.

import (
	"regexp"

	"github.com/stump-wtf/harness/internal/sessionguard"
)

// Class is a model-error class: the value of the `class` label.
type Class string

// The five classes SPEC-0013 REQ-3 allows.
const (
	ClassQuota     Class = "quota"
	ClassAuth      Class = "auth"
	ClassTimeout   Class = "timeout"
	ClassTransport Class = "transport"
	ClassOther     Class = "other"
)

// Classes is every class in exposition order. Every observable harness emits
// each one, zeros included, so an increase() over a class has a series to
// start from before its first error.
var Classes = []Class{ClassQuota, ClassAuth, ClassTimeout, ClassTransport, ClassOther}

// rule maps text matching re to class. Its name is what a park and doctor
// show for the match (SPEC-0021 REQ-13, REQ-17): a fixed label from this file,
// never text from the note, so it can be stored where error text may not be
// (ADR-0008). One rule is one situation; a pattern that once listed two
// (a rate limit and a spent budget) is split, so each name stays true.
type rule struct {
	name  string
	class Class
	re    *regexp.Regexp
}

func rx(class Class, name, pattern string) rule {
	// (?i): provider casing varies ("Rate limit", "RATE_LIMIT_EXCEEDED").
	return rule{name: name, class: class, re: regexp.MustCompile(`(?i)` + pattern)}
}

// litellmRule matches one litellm exception class, named after it: the class
// names are unambiguous whatever the status line says.
func litellmRule(class Class, exception string) rule {
	return rx(class, "litellm."+exception, `litellm\.`+exception)
}

// commonTable names the provider shapes in rule names ("common/rate limit").
const commonTable = "common"

// ruleContextWindow is the rule name of a context-window rejection, which
// Classify recognises through the session guard rather than a table.
const ruleContextWindow = commonTable + "/context window"

// commonRules are provider shapes any adapter can relay. Status codes match as
// whole numbers (\b), so "4290 tokens" is never a 429.
var commonRules = []rule{
	// quota — checked before timeout and transport: "429 ... please retry
	// after the timeout" is a rate limit, not a timeout.
	rx(ClassQuota, "rate limit", `\b429\b|too many requests|rate[ _-]?limit`),
	rx(ClassQuota, "quota exceeded", `insufficient[ _]quota|exceeded your current quota|quota[ _]exceeded|resource[ _]exhausted`),
	rx(ClassQuota, "usage limit", `usage limit|weekly limit|monthly limit|daily limit|spend(ing)? limit|budget exceeded`),
	rx(ClassQuota, "payment required", `\b402\b|payment required|credit balance|insufficient (credits|balance|funds)|billing`),

	// auth — before transport, so a gateway's "403 Forbidden" is auth.
	rx(ClassAuth, "unauthorized", `\b401\b|\b403\b|unauthori[sz]ed|forbidden`),
	rx(ClassAuth, "invalid api key", `invalid[ _-]?(x-)?api[ _-]?key|incorrect api key|api key not valid|(no|missing) api key`),
	rx(ClassAuth, "authentication failed", `authentication[ _]?(error|failed)|permission[ _]?(error|denied)|access denied|token (has )?expired|invalid[ _]token`),

	// transport, ahead of timeout: a timeout while connecting — dialing, the
	// TLS handshake, a proxy's upstream connect — is "could not be reached",
	// not "accepted, then too slow". Without this, every network outage
	// whose error says "timeout" read as the provider being slow.
	//
	// @joestump-agent 09/23/2026 - Added in review (harness#589).
	rx(ClassTransport, "connect timeout", `dial tcp[^\n]*timed? ?out|handshake time-?out|connect(ion)? timed? ?out|upstream connect error`),

	// timeout — accepted, then too slow.
	rx(ClassTimeout, "timeout", `\b408\b|\b504\b|gateway time-?out|request time-?out|timed? ?out|deadline exceeded`),

	// transport — unreachable, or the provider could not serve. Overload is
	// here and not in quota; see the header.
	rx(ClassTransport, "connection failed", `connection (refused|reset|closed|aborted|error)|broken pipe|\beof\b|no such host|network is unreachable|dial tcp|server disconnected`),
	rx(ClassTransport, "tls", `\btls\b|x509|certificate|handshake`),
	rx(ClassTransport, "server error", `\b500\b|\b502\b|\b503\b|\b529\b|bad gateway|service unavailable|internal server error|overloaded`),
}

// adapterRules are each adapter's own phrasings, checked before commonRules.
var adapterRules = map[string][]rule{
	// Crush records a failed turn as a finish part: the message is the HTTP
	// status text, the details the provider body — often litellm's, whose
	// exception class names are unambiguous whatever the status line says.
	"crush": {
		litellmRule(ClassQuota, "ratelimiterror"),
		litellmRule(ClassQuota, "budgetexceedederror"),
		litellmRule(ClassAuth, "authenticationerror"),
		litellmRule(ClassAuth, "permissiondeniederror"),
		litellmRule(ClassTimeout, "timeout"),
		litellmRule(ClassTransport, "apiconnectionerror"),
		litellmRule(ClassTransport, "serviceunavailableerror"),
		litellmRule(ClassTransport, "internalservererror"),
	},
	// Claude Code prints its own summaries over the API error. agent-trace
	// (stump.wtf/agent-trace#104) prefixes each error mark with the record's
	// error code and HTTP status — "rate_limit (429): …", "server_error: …"
	// with no status when the request never got a response — and the code is
	// what names the failure when the text does not.
	"claude-code": {
		// A throttle the provider says is not the account's own limit has no
		// reset time on the account; it clears like overload, so it is
		// transport — checked before the shared 429 → quota rule.
		rx(ClassTransport, "not your usage limit", `not your usage limit`),
		rx(ClassQuota, "usage limit reached", `usage limit reached`),
		rx(ClassQuota, "credit balance is too low", `credit balance is too low`),
		rx(ClassAuth, "login required", `please run /login|oauth token (has )?expired`),
		rx(ClassTimeout, "request timed out", `request timed out|^server_error \((408|504)\)`),
		// Every other server_error is the provider failing to serve: the
		// connection-failure notes carry no status and no phrase the shared
		// table knows ("ConnectionRefused" has no space), so without this
		// they all read as unclassified.
		rx(ClassTransport, "server_error", `^server_error\b`),
	},
	// Codex reports stream and retry failures in its own words.
	"codex": {
		rx(ClassQuota, "usage limit", `you've hit your usage limit`),
		rx(ClassQuota, "rate limit retries exhausted", `exceeded retry limit, last status: 429`),
		rx(ClassAuth, "unauthorized", `unexpected status (401|403)`),
		rx(ClassTransport, "connection failed", `stream disconnected before completion|error sending request`),
	},
}

// Classify maps an agent error note to its class. known is false when nothing
// matched: the class is then ClassOther and the caller also counts the error
// as unclassified. A context-window rejection is recognised but is none of the
// four named situations, so it is ClassOther with known = true — it must never
// feed the unclassified control, or every wedged session would read as a
// provider changing its wording.
func Classify(adapter, note string) (class Class, known bool) {
	class, _, known = ClassifyRule(adapter, note)
	return class, known
}

// ClassifyRule is Classify plus the name of the rule that matched, as
// "<table>/<name>": the table is the adapter whose own rule matched, or
// "common" — "crush/litellm.ratelimiterror", "common/payment required". rule
// is "" when known is false. A park stores the rule, never the note
// (SPEC-0021 REQ-13).
func ClassifyRule(adapter, note string) (class Class, rule string, known bool) {
	// Context-window first: it rides a 400 Bad Request the tables would
	// otherwise leave unmatched, and sharing the session guard's list means
	// the two can never disagree about what one looks like.
	if sessionguard.IsContextError(note) {
		return ClassOther, ruleContextWindow, true
	}
	for _, rl := range adapterRules[adapter] {
		if rl.re.MatchString(note) {
			return rl.class, adapter + "/" + rl.name, true
		}
	}
	for _, rl := range commonRules {
		if rl.re.MatchString(note) {
			return rl.class, commonTable + "/" + rl.name, true
		}
	}
	return ClassOther, "", false
}
