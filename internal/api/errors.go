package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// ErrorCode is a stable machine code from the API's error envelope. Branch on
// this, never on the message.
type ErrorCode string

// The complete set the contract declares (components.schemas.ErrorCode).
const (
	CodeInvalidToken                 ErrorCode = "invalid_token"
	CodeSubscriptionInactive         ErrorCode = "subscription_inactive"
	CodeQuotaExhausted               ErrorCode = "quota_exhausted"
	CodeRateLimited                  ErrorCode = "rate_limited"
	CodeResolutionNotInPlan          ErrorCode = "resolution_not_in_plan"
	CodeUnknownPlace                 ErrorCode = "unknown_place"
	CodeNotCovered                   ErrorCode = "not_covered"
	CodeReportsUnavailableForCountry ErrorCode = "reports_unavailable_for_country"
	CodeReadOnlyToken                ErrorCode = "read_only_token"
	CodeTooManyHexes                 ErrorCode = "too_many_hexes"
	CodeMissingParameter             ErrorCode = "missing_parameter"
	CodeInvalidParameter             ErrorCode = "invalid_parameter"
	CodeInvalidCursor                ErrorCode = "invalid_cursor"
	CodeNotFound                     ErrorCode = "not_found"
	CodeMalformedRequest             ErrorCode = "malformed_request"
	CodeInternalError                ErrorCode = "internal_error"
)

// Process exit codes. Task 27 hands these to os.Exit; they are here so the
// mapping lives next to the codes it is derived from.
//
// 2, 3 and 4 are the three the plan pins. Everything else is 1: a CLI that
// invented a code per error would make every new code a breaking change for
// whoever scripted against it.
const (
	ExitOK         = 0
	ExitError      = 1
	ExitQuota      = 2
	ExitAuth       = 3
	ExitNotCovered = 4
)

// Sentinels, one per code, for errors.Is. They carry nothing but the code.
//
//	if errors.Is(err, api.ErrNotCovered) { … }
var (
	ErrInvalidToken                 = sentinel(CodeInvalidToken)
	ErrSubscriptionInactive         = sentinel(CodeSubscriptionInactive)
	ErrQuotaExhausted               = sentinel(CodeQuotaExhausted)
	ErrRateLimited                  = sentinel(CodeRateLimited)
	ErrResolutionNotInPlan          = sentinel(CodeResolutionNotInPlan)
	ErrUnknownPlace                 = sentinel(CodeUnknownPlace)
	ErrNotCovered                   = sentinel(CodeNotCovered)
	ErrReportsUnavailableForCountry = sentinel(CodeReportsUnavailableForCountry)
	ErrReadOnlyToken                = sentinel(CodeReadOnlyToken)
	ErrTooManyHexes                 = sentinel(CodeTooManyHexes)
	ErrMissingParameter             = sentinel(CodeMissingParameter)
	ErrInvalidParameter             = sentinel(CodeInvalidParameter)
	ErrInvalidCursor                = sentinel(CodeInvalidCursor)
	ErrNotFound                     = sentinel(CodeNotFound)
	ErrMalformedRequest             = sentinel(CodeMalformedRequest)
	ErrInternalError                = sentinel(CodeInternalError)
)

func sentinel(code ErrorCode) *APIError {
	return &APIError{Code: code, isSentinel: true}
}

// APIError is one error envelope, decoded. The server's Message is kept
// VERBATIM — the CLI never rewrites it — and DocsURL is carried so a caller
// cannot drop the one link every envelope publishes.
//
// Fields past the three required ones are additive per the contract, so Extra
// keeps every key as sent: a code that grows a field stays readable without a
// client release.
type APIError struct {
	Code    ErrorCode
	Message string
	DocsURL string

	// Status is the HTTP status. Zero for an error this client raised
	// without sending the request.
	Status int

	// Named fields the contract documents on specific failures.
	Parameter  string       // the 400s that name a parameter
	Selectors  []string     // the one-territory 400: which selectors collided
	RequestID  string       // quote this on a 500
	GeoID      string       // unknown_place from a geo_id
	Query      string       // unknown_place from a search
	DidYouMean []Suggestion // unknown_place from a search — may be empty
	H3         string       // unknown_place from a cell
	ResetsAt   string       // quota_exhausted
	UpgradeURL string       // quota_exhausted
	Group      string       // rate_limited, app layer only
	Limit      *int         // rate_limited, app layer only
	RetryAfter *int         // rate_limited, app layer only, seconds

	// Meta is the response's quota and request headers, empty when the
	// error never reached the API.
	Meta Meta

	// Extra is every key of the envelope, decoded lazily by a caller that
	// needs a field this struct does not name yet.
	Extra map[string]json.RawMessage

	isSentinel bool
}

// Error renders the server's message verbatim, then the machine code and the
// docs link. The link is part of the string on purpose: dropping it makes the
// CLI less useful than curl, and a caller that only prints err still shows it.
func (e *APIError) Error() string {
	var b strings.Builder
	switch {
	case e.Message != "":
		b.WriteString(e.Message)
	case e.Code != "":
		b.WriteString(string(e.Code))
	default:
		b.WriteString("the API returned an error")
	}
	if e.Code != "" && e.Message != "" {
		fmt.Fprintf(&b, " [%s]", e.Code)
	}
	if e.RequestID != "" {
		fmt.Fprintf(&b, " (request_id: %s)", e.RequestID)
	} else if e.Meta.RequestID != "" && e.Status >= 500 {
		fmt.Fprintf(&b, " (request_id: %s)", e.Meta.RequestID)
	}
	if e.DocsURL != "" {
		fmt.Fprintf(&b, " — see %s", e.DocsURL)
	}
	return b.String()
}

// Is matches a sentinel by code, so errors.Is(err, ErrNotCovered) works on a
// fully populated error.
func (e *APIError) Is(target error) bool {
	t, ok := target.(*APIError)
	return ok && t.isSentinel && t.Code == e.Code
}

// ExitCode is the process status this error should end the CLI with.
func (e *APIError) ExitCode() int {
	switch e.Code {
	case CodeQuotaExhausted, CodeSubscriptionInactive:
		// Both are 402: money, not credentials. Sending someone to
		// re-issue a token that was never the problem wastes their day.
		return ExitQuota
	case CodeInvalidToken, CodeReadOnlyToken:
		return ExitAuth
	case CodeNotCovered:
		return ExitNotCovered
	default:
		return ExitError
	}
}

// Retryable reports whether sending the same request again could succeed.
// A 4xx that is not 429 never can.
func (e *APIError) Retryable() bool {
	if e.Code == CodeRateLimited {
		return true
	}
	return e.Status >= 500
}

// Hint is the recovery action for this code, in one sentence. It is ADDED to
// the server's message, never substituted for it. The pairs the contract says
// must not be conflated get visibly different hints.
func (e *APIError) Hint() string {
	switch e.Code {
	case CodeInvalidToken:
		return "Set a valid token: investviews auth login --token …, or INVESTVIEWS_TOKEN."
	case CodeReadOnlyToken:
		return "This token is read-only. Mint a read/write token for this endpoint."
	case CodeSubscriptionInactive:
		return "The subscription that owns this token is not active. The token itself is fine."
	case CodeQuotaExhausted:
		return "This cycle's allowance for the group is spent. Check investviews usage; it refills at the cycle end."
	case CodeRateLimited:
		return "Too many requests this minute — this is a burst limit, NOT your quota. Wait and retry."
	case CodeResolutionNotInPlan:
		// ⚠️ THIS HINT CANNOT SAY "USE A COARSER --res" AND STOP. --res is
		// valid for the circle ALONE: it is refused locally alongside both
		// --h3 and --geo-id (see StatsParams.selector), and a 403 here
		// most often came from --h3 cells finer than the plan reaches. A
		// hint naming a flag the CLI then refuses sends the caller into an
		// error it did not cause, so every selector gets its own move.
		return "This plan does not reach that resolution. With --lat/--lng/--radius-km ask for a " +
			"coarser --res; with --h3 ask about coarser cells; or name the place with --geo-id, " +
			"which is answered as one row and chooses no resolution."
	case CodeUnknownPlace:
		// Name the suggestions only when there are some: "try the
		// did_you_mean suggestions" over an empty list sends the caller
		// looking for something that is not there.
		if len(e.DidYouMean) > 0 {
			return "Nothing matched inside a market we serve. Try the did_you_mean suggestions, or browse from investviews geo."
		}
		return "Nothing matched inside a market we serve. Look the place up by name with `investviews geo search`, or browse from investviews geo."
	case CodeNotCovered:
		return "We hold no data for that country at all. Retrying never succeeds — pick another market; investviews coverage lists them."
	case CodeInvalidCursor:
		return "Your position in this listing is gone. Start the listing again WITHOUT a cursor; retrying the same cursor never succeeds."
	case CodeInvalidParameter:
		return "Fix the value and retry."
	case CodeMissingParameter:
		return "Send the parameter the message names."
	case CodeTooManyHexes:
		// Same rule as resolution_not_in_plan above: this 403's commonest
		// cause is a long --h3 list, and --res is refused alongside --h3.
		return "The territory exceeds the per-request cell budget. Ask about fewer cells with --h3, " +
			"a smaller --radius-km (a coarser --res works for the circle only), or name the place " +
			"with --geo-id, which is one row and has no cell budget."
	case CodeReportsUnavailableForCountry:
		return "The report pipeline does not serve that country."
	case CodeInternalError:
		return "A fault on our side. Retry, and quote the request_id if it persists."
	default:
		return ""
	}
}

// RateLimitScope says which layer refused the request. BOTH layers send
// error: "rate_limited" on purpose, so this is decided on the extra fields,
// never on the code.
type RateLimitScope string

const (
	// RateLimitAccount is the app layer: a real plan burst limit for this
	// account and endpoint group.
	RateLimitAccount RateLimitScope = "account"
	// RateLimitEdge is nginx's per-ADDRESS backstop. It is not about this
	// account at all, and Rails never ran.
	RateLimitEdge RateLimitScope = "edge"
)

// RateLimitError is a 429. Never a quota problem — that is 402
// quota_exhausted — and on the edge scope not even a problem with this
// account.
type RateLimitError struct {
	*APIError
	Scope RateLimitScope
	// Wait is how long to wait: Retry-After when the server sent one, else
	// zero, and the caller backs off on its own.
	Wait     time.Duration
	HasRetry bool
}

func (e *RateLimitError) Unwrap() error { return e.APIError }

// QuotaError is a 402 — quota_exhausted or subscription_inactive. Exit 2.
type QuotaError struct{ *APIError }

func (e *QuotaError) Unwrap() error { return e.APIError }

// AuthError is a credential problem — invalid_token or read_only_token.
// Exit 3. A 402 is NOT one of these.
type AuthError struct{ *APIError }

func (e *AuthError) Unwrap() error { return e.APIError }

// NotCoveredError means this API holds no geographic data for that country.
// Retrying never succeeds; pick another market. Exit 4.
type NotCoveredError struct{ *APIError }

func (e *NotCoveredError) Unwrap() error { return e.APIError }

// UnknownPlaceError means the name, cell or key did not resolve INSIDE a
// market we serve. Distinct from NotCoveredError: this one has a next move,
// and DidYouMean often holds it.
type UnknownPlaceError struct{ *APIError }

func (e *UnknownPlaceError) Unwrap() error { return e.APIError }

// InvalidCursorError means the listing position is gone: start again from no
// cursor. Deliberately NOT an invalid_parameter — a client that treats it as
// one retries the same rejected cursor forever.
type InvalidCursorError struct{ *APIError }

func (e *InvalidCursorError) Unwrap() error { return e.APIError }

// ValidationError is a request this client refused to send, so it costs no
// quota and no round trip. The API enforces the same rules server-side.
type ValidationError struct {
	Parameter string
	Selectors []string
	Message   string
}

func (e *ValidationError) Error() string { return e.Message }

// ExitCode keeps a locally refused request on the generic failure code: it is
// a usage mistake, not a quota, auth or coverage answer.
func (e *ValidationError) ExitCode() int { return ExitError }

// ErrNoPaging is returned by an All-style call on an endpoint that does not
// page. /geo/search is the only one: it takes a limit and nothing else, and
// the server clamps that. A --all there would appear to work and quietly
// return one page.
var ErrNoPaging = errors.New("this endpoint returns one page and cannot be paged")

// ErrPagingStalled is returned when offset paging stops making progress — the
// same rows came back for a second page. Reported rather than swallowed: a
// silent break would look like the end of the listing.
var ErrPagingStalled = errors.New("paging stopped making progress")

// ExitCode is the process status for any error this package can produce.
func ExitCode(err error) int {
	if err == nil {
		return ExitOK
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.ExitCode()
	}
	var valErr *ValidationError
	if errors.As(err, &valErr) {
		return valErr.ExitCode()
	}
	return ExitError
}

// AsAPIError pulls the envelope out of any error this package returns.
func AsAPIError(err error) (*APIError, bool) {
	var apiErr *APIError
	ok := errors.As(err, &apiErr)
	return apiErr, ok
}

// Suggestion is one entry of did_you_mean on an unknown_place 404.
//
// ⚠️ IT IS AN OBJECT, NOT A NAME. Each suggestion carries a geo_id that can be
// spent straight away on ?parent= or /stats/current?geo_id= — which is the
// whole value of the field, and the reason a name alone would be useless:
// 13.6% of neighbourhood names repeat inside their own country.
//
// This was typed []string until 2026-09-12. The empty case [] decoded into
// that perfectly, and the ground-truth sample only ever recorded the empty
// case, so nothing looked wrong until a live miss returned objects. See
// parseError for the second half of that bug.
//
// ⚠️ Ancestors here is not guaranteed to end on a country entry: one sample
// stopped at region, while a live 2026-09-12 miss ended on a country. Read
// each step's level; never infer a level from its position in the chain.
type Suggestion struct {
	GeoID     string    `json:"geo_id"`
	Name      string    `json:"name"`
	Level     string    `json:"level"`
	Country   string    `json:"country"`
	Ancestors Ancestors `json:"ancestors"`
}

// parseError turns a non-2xx response body into a typed error. A body that is
// not the JSON envelope (a proxy's HTML page, an empty 502) still produces an
// APIError, with the status standing in for the code.
//
// ⚠️ EVERY FIELD IS DECODED INDEPENDENTLY, and a field whose shape this client
// does not expect is dropped rather than failing the whole envelope.
//
// did_you_mean taught this on 2026-09-12. It was typed []string, the API sends
// objects, and because the envelope was decoded in ONE strict pass, that single
// mismatch fell through to the not-JSON fallback: every unknown_place 404 was
// reported as not_found, with no message, no hint and no suggestions. The three
// required fields — error, message, docs_url — must survive whatever an
// optional one does, and the raw bytes of anything that would not decode stay
// in Extra.
func parseError(status int, header http.Header, body []byte, meta Meta) error {
	apiErr := &APIError{Status: status, Meta: meta, RequestID: meta.RequestID}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil || rawString(fields, "error") == "" {
		apiErr.Code = codeForStatus(status)
		apiErr.Message = fallbackMessage(status, body)
		return classify(apiErr, header)
	}

	apiErr.Extra = fields
	apiErr.Code = ErrorCode(rawString(fields, "error"))
	apiErr.Message = rawString(fields, "message")
	apiErr.DocsURL = rawString(fields, "docs_url")
	apiErr.Parameter = rawString(fields, "parameter")
	apiErr.GeoID = rawString(fields, "geo_id")
	apiErr.Query = rawString(fields, "query")
	apiErr.H3 = rawString(fields, "h3")
	apiErr.ResetsAt = rawString(fields, "resets_at")
	apiErr.UpgradeURL = rawString(fields, "upgrade_url")
	apiErr.Group = rawString(fields, "group")
	apiErr.Limit = rawInt(fields, "limit")
	apiErr.RetryAfter = rawInt(fields, "retry_after")
	apiErr.Selectors = rawStrings(fields, "selectors")
	apiErr.DidYouMean = rawSuggestions(fields, "did_you_mean")
	if id := rawString(fields, "request_id"); id != "" {
		apiErr.RequestID = id
	}
	return classify(apiErr, header)
}

// rawString reads a field as a string, accepting a JSON number too — h3 ids
// travel as decimal strings, but a number there is not worth losing the
// envelope over. Any other shape reads as empty.
func rawString(fields map[string]json.RawMessage, key string) string {
	return jsonScalarString(fields[key])
}

func rawInt(fields map[string]json.RawMessage, key string) *int {
	raw, ok := fields[key]
	if !ok {
		return nil
	}
	var n int
	if err := json.Unmarshal(raw, &n); err != nil {
		return nil
	}
	return &n
}

func rawStrings(fields map[string]json.RawMessage, key string) []string {
	raw, ok := fields[key]
	if !ok {
		return nil
	}
	var out []string
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}
	return out
}

// rawSuggestions decodes did_you_mean. A shape this client does not expect
// costs the suggestions and NOTHING ELSE — the envelope, the code and the hint
// all still reach the user, and the raw bytes stay in Extra.
func rawSuggestions(fields map[string]json.RawMessage, key string) []Suggestion {
	raw, ok := fields[key]
	if !ok {
		return nil
	}
	var out []Suggestion
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}
	return out
}

// classify wraps the envelope in the type that carries its recovery action.
func classify(apiErr *APIError, header http.Header) error {
	switch apiErr.Code {
	case CodeRateLimited:
		wait, ok := retryAfterFrom(header, apiErr)
		return &RateLimitError{
			APIError: apiErr,
			Scope:    rateLimitScope(apiErr, header),
			Wait:     wait,
			HasRetry: ok,
		}
	case CodeQuotaExhausted, CodeSubscriptionInactive:
		return &QuotaError{apiErr}
	case CodeInvalidToken, CodeReadOnlyToken:
		return &AuthError{apiErr}
	case CodeNotCovered:
		return &NotCoveredError{apiErr}
	case CodeUnknownPlace:
		return &UnknownPlaceError{apiErr}
	case CodeInvalidCursor:
		return &InvalidCursorError{apiErr}
	default:
		return apiErr
	}
}

// rateLimitScope tells the two 429s apart.
//
// ⚠️ NEVER branch on the code: both layers send error: "rate_limited" on
// purpose, so that a client needs one branch for "slow down". The app layer
// adds group/limit/retry_after to the body and the X-Quota-*/X-RateLimit-*
// headers; nginx generates its body itself and Rails never runs, so it has
// none of them.
func rateLimitScope(apiErr *APIError, header http.Header) RateLimitScope {
	if apiErr.Group != "" || apiErr.Limit != nil || apiErr.RetryAfter != nil {
		return RateLimitAccount
	}
	if header.Get("X-Quota-Group") != "" || header.Get("X-RateLimit-Limit") != "" {
		return RateLimitAccount
	}
	return RateLimitEdge
}

// retryAfterFrom prefers the Retry-After header, then the body's retry_after.
// Both layers send the header; only the app layer sends the body field.
func retryAfterFrom(header http.Header, apiErr *APIError) (time.Duration, bool) {
	if d, ok := parseRetryAfter(header.Get("Retry-After"), time.Now()); ok {
		return d, true
	}
	if apiErr != nil && apiErr.RetryAfter != nil && *apiErr.RetryAfter >= 0 {
		return time.Duration(*apiErr.RetryAfter) * time.Second, true
	}
	return 0, false
}

// parseRetryAfter reads both forms RFC 9110 allows: seconds, or an HTTP date.
func parseRetryAfter(value string, now time.Time) (time.Duration, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	if secs, err := parseInt(value); err == nil {
		if secs < 0 {
			return 0, false
		}
		return time.Duration(secs) * time.Second, true
	}
	if at, err := http.ParseTime(value); err == nil {
		d := at.Sub(now)
		if d < 0 {
			d = 0
		}
		return d, true
	}
	return 0, false
}

// codeForStatus is the last resort when the body is not the JSON envelope.
func codeForStatus(status int) ErrorCode {
	switch {
	case status == http.StatusUnauthorized:
		return CodeInvalidToken
	case status == http.StatusPaymentRequired:
		return CodeSubscriptionInactive
	case status == http.StatusTooManyRequests:
		return CodeRateLimited
	case status == http.StatusNotFound:
		return CodeNotFound
	case status >= 500:
		return CodeInternalError
	default:
		return CodeMalformedRequest
	}
}

func fallbackMessage(status int, body []byte) string {
	text := strings.TrimSpace(string(body))
	if len(text) > 300 {
		text = text[:300] + "…"
	}
	if text == "" {
		return fmt.Sprintf("HTTP %d %s, with an empty body", status, http.StatusText(status))
	}
	return fmt.Sprintf("HTTP %d %s, and the body was not the API's JSON error envelope: %s",
		status, http.StatusText(status), text)
}

// jsonScalarString reads a value the API may send as a string or a number.
// Any other shape — an object, an array — reads as empty rather than as its
// own source text.
func jsonScalarString(raw json.RawMessage) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return ""
	}
	var s string
	if err := json.Unmarshal(trimmed, &s); err == nil {
		return s
	}
	var n json.Number
	if err := json.Unmarshal(trimmed, &n); err == nil {
		return n.String()
	}
	return ""
}
