package provider

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Provider names accepted in configuration.
const (
	ProviderGemini           = "gemini"
	ProviderOpenAICompatible = "openai_compatible"
	ProviderAnthropic        = "anthropic"
	ProviderCustom           = "custom"
)

// Settings configure a provider client. They are produced from .env and
// config.json by the config package.
type Settings struct {
	Provider string // gemini | openai_compatible | anthropic | custom
	// APIFormat selects the wire format when Provider is "custom"
	// (gemini | openai | anthropic). Ignored otherwise.
	APIFormat string
	// BaseURL is normalised per provider (see Endpoint). For "custom" it is the
	// exact URL requests are POSTed to.
	BaseURL string
	APIKey  string
	Model   string

	// AuthStyle overrides the default credential header:
	// bearer | x-api-key | x-goog-api-key | none. Empty means the family default.
	AuthStyle    string
	ExtraHeaders map[string]string
	// ExtraBody is deep-merged into every request body, an escape hatch for
	// provider-specific knobs (thinking budgets, safety settings, ...).
	ExtraBody map[string]any

	// GeminiToolMedia controls where media returned by a tool is placed:
	// "inline" (inside functionResponse.parts, Gemini 3) or "sibling".
	GeminiToolMedia string
	// OpenAIStreamUsage asks for token usage in the final stream chunk.
	OpenAIStreamUsage bool
	// OpenAIReplayReasoning echoes reasoning_content back on assistant turns.
	OpenAIReplayReasoning bool
	AnthropicVersion      string
	// PromptCache places Anthropic cache_control breakpoints.
	PromptCache bool
	// AnthropicMaxTokens is the mandatory output cap when none is requested.
	AnthropicMaxTokens int

	StallTimeout time.Duration
}

// Format returns the wire format implied by the settings.
func (s Settings) Format() (string, error) {
	switch s.Provider {
	case ProviderGemini:
		return FormatGemini, nil
	case ProviderOpenAICompatible:
		return FormatOpenAI, nil
	case ProviderAnthropic:
		return FormatAnthropic, nil
	case ProviderCustom:
		switch s.APIFormat {
		case FormatGemini, FormatOpenAI, FormatAnthropic:
			return s.APIFormat, nil
		}
		return "", fmt.Errorf("provider custom needs API_FORMAT=gemini|openai|anthropic, got %q", s.APIFormat)
	}
	return "", fmt.Errorf("unknown provider %q (use gemini, openai_compatible, anthropic or custom)", s.Provider)
}

// Endpoint computes the request URL for a streaming call.
//
// Normalisation rules (custom is always exact):
//
//	gemini            base path empty        -> /v1beta ; then /models/<model>:streamGenerateContent?alt=sse
//	openai_compatible base path empty        -> /v1     ; then /chat/completions (unless already present)
//	anthropic         base path empty        -> /v1     ; then /messages (unless already present)
//
// A base that already carries a path (for example /v1beta/openai or /api/v3)
// is respected as given, so non-standard gateways keep working.
func Endpoint(s Settings) (string, error) {
	format, err := s.Format()
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(s.BaseURL) == "" && s.Provider != ProviderGemini && s.Provider != ProviderAnthropic {
		return "", fmt.Errorf("BASE_URL is required for provider %s", s.Provider)
	}
	if s.Provider == ProviderCustom {
		u, err := url.Parse(strings.TrimSpace(s.BaseURL))
		if err != nil || u.Scheme == "" || u.Host == "" {
			return "", fmt.Errorf("custom BASE_URL must be an absolute URL, got %q", s.BaseURL)
		}
		return strings.TrimSpace(s.BaseURL), nil
	}

	base := strings.TrimSpace(s.BaseURL)
	if base == "" {
		switch format {
		case FormatGemini:
			base = "https://generativelanguage.googleapis.com"
		case FormatAnthropic:
			base = "https://api.anthropic.com"
		}
	}
	u, err := url.Parse(base)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("BASE_URL must be an absolute URL, got %q", s.BaseURL)
	}
	path := strings.TrimRight(u.Path, "/")

	switch format {
	case FormatGemini:
		if path == "" {
			path = "/v1beta"
		}
		model := strings.TrimPrefix(s.Model, "models/")
		if model == "" {
			return "", fmt.Errorf("MODEL is required")
		}
		u.Path = path + "/models/" + model + ":streamGenerateContent"
		u.RawQuery = "alt=sse"
	case FormatOpenAI:
		if path == "" {
			path = "/v1"
		}
		if !strings.HasSuffix(path, "/chat/completions") {
			path += "/chat/completions"
		}
		u.Path = path
	case FormatAnthropic:
		if path == "" {
			path = "/v1"
		}
		if !strings.HasSuffix(path, "/messages") {
			path += "/messages"
		}
		u.Path = path
	}
	return u.String(), nil
}

// New builds a ready-to-use Client from settings.
func New(s Settings) (*Client, error) {
	format, err := s.Format()
	if err != nil {
		return nil, err
	}
	endpoint, err := Endpoint(s)
	if err != nil {
		return nil, err
	}
	hdr := func() http.Header { return authHeaders(s, format) }

	var ad Adapter
	switch format {
	case FormatGemini:
		ad = &geminiAdapter{s: s, endpoint: endpoint, header: hdr}
	case FormatOpenAI:
		ad = &openAIAdapter{s: s, endpoint: endpoint, header: hdr}
	case FormatAnthropic:
		ad = &anthropicAdapter{s: s, endpoint: endpoint, header: hdr}
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.ResponseHeaderTimeout = 120 * time.Second
	return &Client{
		HTTP:         &http.Client{Transport: tr},
		Adapter:      ad,
		StallTimeout: s.StallTimeout,
	}, nil
}

func authHeaders(s Settings, format string) http.Header {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("Accept", "text/event-stream")
	style := s.AuthStyle
	if style == "" {
		switch format {
		case FormatGemini:
			style = "x-goog-api-key"
		case FormatAnthropic:
			style = "x-api-key"
		default:
			style = "bearer"
		}
	}
	if s.APIKey != "" {
		switch style {
		case "bearer":
			h.Set("Authorization", "Bearer "+s.APIKey)
		case "x-api-key":
			h.Set("x-api-key", s.APIKey)
		case "x-goog-api-key":
			h.Set("x-goog-api-key", s.APIKey)
		}
	}
	if format == FormatAnthropic {
		v := s.AnthropicVersion
		if v == "" {
			v = "2023-06-01"
		}
		h.Set("anthropic-version", v)
	}
	for k, v := range s.ExtraHeaders {
		h.Set(k, v)
	}
	return h
}

// mergeExtra deep-merges extra into body (extra wins on scalar conflicts).
func mergeExtra(body map[string]any, extra map[string]any) {
	for k, v := range extra {
		if ev, ok := v.(map[string]any); ok {
			if bv, ok := body[k].(map[string]any); ok {
				mergeExtra(bv, ev)
				continue
			}
		}
		body[k] = v
	}
}

// marshalBody encodes the request body. encoding/json sorts map keys, so the
// output is deterministic for identical input, which keeps provider prompt
// caches warm across turns.
func marshalBody(body map[string]any) ([]byte, error) { return encodeJSON(body) }

// encodeJSON marshals v without HTML escaping (keeps <, >, & literal: smaller,
// more readable bodies) and without the encoder's trailing newline.
func encodeJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}
