// Package provider turns the engine's canonical conversation into provider
// wire requests and parses streamed responses back. One Adapter exists per
// API family (Gemini, OpenAI-compatible, Anthropic); a Client adds the shared
// HTTP behaviour: error classification, stall detection and cancellation.
package provider

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/ranjanlive/zpilot/zengine/internal/llm"
)

// Format names an API family.
const (
	FormatGemini    = "gemini"
	FormatOpenAI    = "openai"
	FormatAnthropic = "anthropic"
)

// BlobLoader resolves attachment payloads referenced by llm.Media.Blob.
type BlobLoader interface {
	Load(id string) ([]byte, error)
}

// Request is everything needed to render one model call.
type Request struct {
	System          string
	Messages        []llm.Message
	Tools           []llm.ToolSpec
	Model           string
	MaxOutputTokens int
	Temperature     *float64
	Caps            llm.Capabilities
	Blobs           BlobLoader
	// ForbidToolCalls keeps the tool declarations in the request (providers
	// reject histories containing tool calls without them) but tells the model
	// not to call any. Used for compaction summaries.
	ForbidToolCalls bool
}

// Built is a fully encoded HTTP request. Measuring len(Body) before sending is
// how the engine enforces request-size limits.
type Built struct {
	Method string
	URL    string
	Header http.Header
	Body   []byte
	Format string
}

// DeltaKind labels streamed increments for UIs.
type DeltaKind string

const (
	DeltaText     DeltaKind = "text"
	DeltaThought  DeltaKind = "thought"
	DeltaToolCall DeltaKind = "tool_call" // a tool call began; Text holds its name
)

// Delta is a streamed increment.
type Delta struct {
	Kind DeltaKind
	Text string
}

// Response is the assembled outcome of one streamed model call.
type Response struct {
	Message llm.Message
	Usage   *llm.Usage
}

// Adapter encodes requests and decodes streams for one API family.
type Adapter interface {
	Format() string
	Build(req Request) (*Built, error)
	// ParseStream consumes a successful streaming response body.
	ParseStream(r io.Reader, sink func(Delta)) (*Response, error)
}

// ErrKind classifies failures so the engine can choose a remedy.
type ErrKind int

const (
	ErrOther        ErrKind = iota
	ErrRateLimit            // 429 / quota: retry later
	ErrServer               // 5xx / overloaded: retry
	ErrNetwork              // connection reset, DNS, TLS, timeouts: retry
	ErrStalled              // stream went silent: retry
	ErrTooLarge             // 413: shrink the request, then retry
	ErrContextLimit         // prompt exceeds the model window: compact, then retry
	ErrAuth                 // 401/403: do not retry, tell the user
	ErrBadRequest           // 400/404/422: the request itself is wrong
	ErrCanceled             // caller cancelled
)

func (k ErrKind) String() string {
	switch k {
	case ErrRateLimit:
		return "rate_limit"
	case ErrServer:
		return "server"
	case ErrNetwork:
		return "network"
	case ErrStalled:
		return "stalled"
	case ErrTooLarge:
		return "too_large"
	case ErrContextLimit:
		return "context_limit"
	case ErrAuth:
		return "auth"
	case ErrBadRequest:
		return "bad_request"
	case ErrCanceled:
		return "canceled"
	}
	return "other"
}

// Error is a classified provider failure.
type Error struct {
	Kind       ErrKind
	Status     int
	Message    string
	RetryAfter time.Duration
	Cause      error
}

func (e *Error) Error() string {
	if e.Status != 0 {
		return fmt.Sprintf("provider error (%s, HTTP %d): %s", e.Kind, e.Status, e.Message)
	}
	return fmt.Sprintf("provider error (%s): %s", e.Kind, e.Message)
}

func (e *Error) Unwrap() error { return e.Cause }

// Retryable reports whether waiting and re-sending the same request can help.
func (e *Error) Retryable() bool {
	switch e.Kind {
	case ErrRateLimit, ErrServer, ErrNetwork, ErrStalled:
		return true
	}
	return false
}

// AsError extracts a *Error from err, wrapping unknown errors as ErrOther.
func AsError(err error) *Error {
	var pe *Error
	if errors.As(err, &pe) {
		return pe
	}
	if errors.Is(err, context.Canceled) {
		return &Error{Kind: ErrCanceled, Message: "cancelled", Cause: err}
	}
	return &Error{Kind: ErrOther, Message: err.Error(), Cause: err}
}

// classifyHTTP maps a non-2xx response to an Error.
func classifyHTTP(status int, header http.Header, body []byte) *Error {
	msg := strings.TrimSpace(string(body))
	if len(msg) > 2000 {
		msg = msg[:2000] + "…"
	}
	e := &Error{Status: status, Message: msg}
	if ra := header.Get("Retry-After"); ra != "" {
		if secs, err := strconv.Atoi(strings.TrimSpace(ra)); err == nil {
			e.RetryAfter = time.Duration(secs) * time.Second
		}
	}
	lower := strings.ToLower(msg)
	switch {
	case status == http.StatusRequestEntityTooLarge || strings.Contains(lower, "request body is too large") || strings.Contains(lower, "request entity too large"):
		e.Kind = ErrTooLarge
	case status == http.StatusTooManyRequests || strings.Contains(lower, "resource_exhausted") || strings.Contains(lower, "rate limit"):
		e.Kind = ErrRateLimit
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		e.Kind = ErrAuth
	case status == http.StatusRequestTimeout || status == 529 || status >= 500:
		e.Kind = ErrServer
	case status >= 400 && isContextOverflow(lower):
		e.Kind = ErrContextLimit
	case status >= 400:
		e.Kind = ErrBadRequest
	default:
		e.Kind = ErrOther
	}
	return e
}

func isContextOverflow(lower string) bool {
	for _, needle := range []string{
		"context length", "context window", "maximum context", "too many tokens",
		"prompt is too long", "input token count", "exceeds the maximum number of tokens",
		"token limit", "tokens exceed",
	} {
		if strings.Contains(lower, needle) {
			return true
		}
	}
	return false
}

// Client executes Built requests with the shared HTTP behaviour.
type Client struct {
	HTTP    *http.Client
	Adapter Adapter
	// StallTimeout aborts a response that delivers no bytes for this long.
	// Zero disables the watchdog.
	StallTimeout time.Duration
}

// Send executes the request and streams the response through the adapter.
func (c *Client) Send(ctx context.Context, b *Built, sink func(Delta)) (*Response, error) {
	if sink == nil {
		sink = func(Delta) {}
	}
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	req, err := http.NewRequestWithContext(ctx, b.Method, b.URL, bytes.NewReader(b.Body))
	if err != nil {
		return nil, &Error{Kind: ErrOther, Message: err.Error(), Cause: err}
	}
	req.Header = b.Header.Clone()
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, c.wrapTransportErr(ctx, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return nil, classifyHTTP(resp.StatusCode, resp.Header, body)
	}

	wd := newWatchdog(c.StallTimeout, func() { cancel(errStalled) })
	defer wd.stop()
	out, perr := c.Adapter.ParseStream(&watchedReader{r: resp.Body, wd: wd}, sink)
	if perr != nil {
		return nil, c.wrapTransportErr(ctx, perr)
	}
	return out, nil
}

var errStalled = errors.New("stream stalled")

func (c *Client) wrapTransportErr(ctx context.Context, err error) error {
	var pe *Error
	if errors.As(err, &pe) {
		return pe
	}
	if cause := context.Cause(ctx); errors.Is(cause, errStalled) {
		return &Error{Kind: ErrStalled, Message: fmt.Sprintf("no data received for %s", c.StallTimeout), Cause: err}
	}
	if errors.Is(err, context.Canceled) || ctx.Err() == context.Canceled {
		return &Error{Kind: ErrCanceled, Message: "cancelled", Cause: err}
	}
	return &Error{Kind: ErrNetwork, Message: err.Error(), Cause: err}
}

// watchdog fires fn if poke is not called within d.
type watchdog struct {
	timer *time.Timer
	d     time.Duration
	done  atomic.Bool
}

func newWatchdog(d time.Duration, fn func()) *watchdog {
	w := &watchdog{d: d}
	if d > 0 {
		w.timer = time.AfterFunc(d, func() {
			if !w.done.Load() {
				fn()
			}
		})
	}
	return w
}

func (w *watchdog) poke() {
	if w.timer != nil {
		w.timer.Reset(w.d)
	}
}

func (w *watchdog) stop() {
	w.done.Store(true)
	if w.timer != nil {
		w.timer.Stop()
	}
}

type watchedReader struct {
	r  io.Reader
	wd *watchdog
}

func (w *watchedReader) Read(p []byte) (int, error) {
	n, err := w.r.Read(p)
	if n > 0 {
		w.wd.poke()
	}
	return n, err
}
