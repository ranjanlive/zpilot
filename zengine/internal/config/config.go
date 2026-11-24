package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/ranjanlive/zpilot/zengine/internal/llm"
	"github.com/ranjanlive/zpilot/zengine/internal/provider"
)

// Flag is a boolean that also accepts 0/1, "true"/"false", "yes"/"no" and
// "on"/"off" in JSON, so config.json can use either style.
type Flag bool

func (f *Flag) UnmarshalJSON(b []byte) error {
	s := strings.Trim(strings.ToLower(strings.TrimSpace(string(b))), `"`)
	v, err := parseBool(s)
	if err != nil {
		return fmt.Errorf("expected true/false or 1/0, got %s", string(b))
	}
	*f = Flag(v)
	return nil
}

func (f Flag) MarshalJSON() ([]byte, error) { return json.Marshal(bool(f)) }

func parseBool(s string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "yes", "on", "y":
		return true, nil
	case "0", "false", "no", "off", "n", "":
		return false, nil
	}
	return false, fmt.Errorf("not a boolean: %q", s)
}

// Env is the content of the .env file: where the model lives and what it can
// consume. Anything that is not a credential or a model fact belongs in
// config.json instead.
type Env struct {
	Provider  string // gemini | openai_compatible | anthropic | custom
	APIFormat string // custom only: gemini | openai | anthropic
	BaseURL   string
	APIKey    string
	Model     string

	ContextLimit int // model context window in tokens (0 = unknown)
	// CompactAt is the prompt size, in tokens, at which the engine compacts.
	// It may be written as a percentage of ContextLimit ("80%").
	CompactAt int

	Caps llm.Capabilities
}

// Config is config.json. Every field has a default, so the file may be empty.
type Config struct {
	Agent       AgentConfig       `json:"agent"`
	Tools       ToolsConfig       `json:"tools"`
	Context     ContextConfig     `json:"context"`
	Network     NetworkConfig     `json:"network"`
	Permissions PermissionsConfig `json:"permissions"`
	Provider    ProviderTuning    `json:"provider"`
	Service     ServiceConfig     `json:"service"`
}

// ServiceConfig governs the background process that keeps a session alive
// behind the terminal and web interfaces.
type ServiceConfig struct {
	// IdleExitMinutes ends the service after this long with the agent idle, no
	// interface attached and no background task running. 0 = keep running
	// until told to quit.
	IdleExitMinutes int `json:"idle_exit_minutes"`
}

type AgentConfig struct {
	// MaxToolCallTurn bounds model→tools→model iterations inside one prompt.
	MaxToolCallTurn int      `json:"max_tool_call_turn"`
	MaxOutputTokens int      `json:"max_output_tokens"`
	Temperature     *float64 `json:"temperature"`
	// MaxBackgroundWaitMinutes is how long a headless run waits for running
	// background tasks after the model finishes, so it can react to their results.
	MaxBackgroundWaitMinutes int `json:"max_background_wait_minutes"`
}

type ToolsConfig struct {
	// Disabled removes tools (built-in or external) by name.
	Disabled []string `json:"disabled"`
	// Enabled, when non-empty, is an allow-list: only these tools load.
	Enabled               []string `json:"enabled"`
	DefaultTimeoutSeconds int      `json:"default_timeout_seconds"`
	MaxOutputBytes        int      `json:"max_output_bytes"`
	// MaxViewBytes caps how much of a text file view_file will load.
	MaxViewBytes int64 `json:"max_view_bytes"`
	// MaxAttachMB caps a single media attachment (image, audio, pdf, video).
	MaxAttachMB float64 `json:"max_attach_mb"`
}

type ContextConfig struct {
	// MaxTurnToKeepAttachment prunes inline attachments older than this many
	// user turns. 0 keeps them until compaction.
	MaxTurnToKeepAttachment int `json:"max_turn_to_keep_attachment"`
	// AttachmentPruneBatchMB batches age-based pruning so the cached request
	// prefix changes rarely instead of every turn.
	AttachmentPruneBatchMB float64 `json:"attachment_prune_batch_mb"`
	// RequestBodyLimitMB is the largest request body the endpoint accepts.
	RequestBodyLimitMB float64 `json:"request_body_limit_mb"`
	// AutocompactOnRequestBodyLimitExceed compacts when the next request would exceed the limit.
	AutocompactOnRequestBodyLimitExceed Flag `json:"autocompact_on_request_body_limit_exceed"`
	// RemoveOldAttachmentOnRequestBodyLimit drops the oldest attachments first
	// when the next request would exceed the limit. When both flags are on,
	// removal is tried first and compaction is the fallback.
	RemoveOldAttachmentOnRequestBodyLimit Flag `json:"remove_old_attachment_on_request_body_limit"`
	// CompactKeepLastTurns keeps this many recent user prompts verbatim (text
	// only) after a compaction.
	CompactKeepLastTurns int `json:"compact_keep_last_turns"`
}

type NetworkConfig struct {
	MaxRetries           int  `json:"max_retries"`
	RetryInitialMS       int  `json:"retry_initial_ms"`
	RetryMaxMS           int  `json:"retry_max_ms"`
	StallTimeoutSeconds  int  `json:"stall_timeout_seconds"`
	AutoPause            Flag `json:"auto_pause"`
	PauseRetryMinutes    int  `json:"pause_retry_minutes"`
	MaxPauses            int  `json:"max_pauses"`
	EmptyResponseRetries int  `json:"empty_response_retries"`
}

type PermissionsConfig struct {
	// RunCommand: ask | allow | deny. "ask" needs an interactive UI; headless
	// runs treat it as deny and say how to enable it.
	RunCommand string `json:"run_command"`
	// WriteOutsideWorkspace lets file tools write outside the workspace.
	WriteOutsideWorkspace Flag `json:"write_outside_workspace"`
	ReadOutsideWorkspace  Flag `json:"read_outside_workspace"`
}

// ProviderTuning holds wire-level options that do not belong in .env.
type ProviderTuning struct {
	AuthStyle             string            `json:"auth_style"`
	ExtraHeaders          map[string]string `json:"extra_headers"`
	ExtraBody             map[string]any    `json:"extra_body"`
	GeminiToolMedia       string            `json:"gemini_tool_media"`
	OpenAIStreamUsage     Flag              `json:"openai_stream_usage"`
	OpenAIReplayReasoning Flag              `json:"openai_replay_reasoning"`
	PromptCache           Flag              `json:"prompt_cache"`
	AnthropicVersion      string            `json:"anthropic_version"`
	AnthropicMaxTokens    int               `json:"anthropic_max_tokens"`
}

// Defaults returns the built-in configuration.
func Defaults() Config {
	return Config{
		Agent: AgentConfig{MaxToolCallTurn: 200, MaxBackgroundWaitMinutes: 60},
		Tools: ToolsConfig{DefaultTimeoutSeconds: 120, MaxOutputBytes: 64 * 1024, MaxViewBytes: 2 << 20, MaxAttachMB: 20},
		Context: ContextConfig{
			AttachmentPruneBatchMB:                4,
			RequestBodyLimitMB:                    28,
			AutocompactOnRequestBodyLimitExceed:   true,
			RemoveOldAttachmentOnRequestBodyLimit: true,
			CompactKeepLastTurns:                  1,
		},
		Network: NetworkConfig{
			MaxRetries: 6, RetryInitialMS: 1000, RetryMaxMS: 30000, StallTimeoutSeconds: 120,
			AutoPause: true, PauseRetryMinutes: 5, MaxPauses: 12, EmptyResponseRetries: 2,
		},
		Permissions: PermissionsConfig{RunCommand: "ask"},
		Service:     ServiceConfig{IdleExitMinutes: 30},
		Provider: ProviderTuning{
			GeminiToolMedia: "inline", OpenAIStreamUsage: true, PromptCache: true, AnthropicMaxTokens: 8192,
		},
	}
}

// LoadConfig reads config.json over the defaults. A missing file is fine;
// unknown keys are rejected so typos do not silently do nothing.
func LoadConfig(path string) (Config, error) {
	cfg := Defaults()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return cfg, err
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return cfg, nil
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return Defaults(), fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

// Validate returns every problem found in the configuration at once.
func (c Config) Validate() []string {
	var p []string
	if c.Agent.MaxToolCallTurn < 1 {
		p = append(p, "agent.max_tool_call_turn must be >= 1")
	}
	if c.Tools.DefaultTimeoutSeconds < 1 {
		p = append(p, "tools.default_timeout_seconds must be >= 1")
	}
	if c.Context.RequestBodyLimitMB <= 0 {
		p = append(p, "context.request_body_limit_mb must be > 0")
	}
	if c.Context.MaxTurnToKeepAttachment < 0 {
		p = append(p, "context.max_turn_to_keep_attachment must be >= 0 (0 = keep until compaction)")
	}
	if c.Context.CompactKeepLastTurns < 0 {
		p = append(p, "context.compact_keep_last_turns must be >= 0")
	}
	switch c.Permissions.RunCommand {
	case "ask", "allow", "deny":
	default:
		p = append(p, `permissions.run_command must be "ask", "allow" or "deny"`)
	}
	switch c.Provider.GeminiToolMedia {
	case "inline", "sibling":
	default:
		p = append(p, `provider.gemini_tool_media must be "inline" or "sibling"`)
	}
	switch c.Provider.AuthStyle {
	case "", "bearer", "x-api-key", "x-goog-api-key", "none":
	default:
		p = append(p, `provider.auth_style must be one of bearer, x-api-key, x-goog-api-key, none`)
	}
	if c.Network.MaxRetries < 0 || c.Network.RetryInitialMS < 1 || c.Network.RetryMaxMS < c.Network.RetryInitialMS {
		p = append(p, "network retry settings are inconsistent (max_retries>=0, retry_initial_ms>=1, retry_max_ms>=retry_initial_ms)")
	}
	return p
}

// BodyLimitBytes is the request-size ceiling in bytes.
func (c Config) BodyLimitBytes() int { return int(c.Context.RequestBodyLimitMB * 1024 * 1024) }

// LoadEnv reads .env (a missing file is not an error) and applies ZAGENT_*
// process-environment overrides, so secrets can come from the environment.
func LoadEnv(path string) (Env, error) {
	raw, err := ParseDotenv(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return Env{}, err
	}
	if raw == nil {
		raw = map[string]string{}
	}
	for _, k := range []string{"PROVIDER", "API_FORMAT", "BASE_URL", "API_KEY", "MODEL", "CONTEXT_LIMIT", "COMPACT_AT", "VISION", "AUDIO", "PDF", "VIDEO"} {
		if v, ok := os.LookupEnv("ZAGENT_" + k); ok {
			raw[k] = v
		}
	}
	var e Env
	e.Provider = strings.ToLower(strings.TrimSpace(raw["PROVIDER"]))
	e.APIFormat = strings.ToLower(strings.TrimSpace(raw["API_FORMAT"]))
	e.BaseURL = strings.TrimSpace(raw["BASE_URL"])
	e.APIKey = strings.TrimSpace(raw["API_KEY"])
	e.Model = strings.TrimSpace(raw["MODEL"])

	var problems []string
	if s := strings.TrimSpace(raw["CONTEXT_LIMIT"]); s != "" {
		n, err := parseTokens(s)
		if err != nil {
			problems = append(problems, "CONTEXT_LIMIT: "+err.Error())
		}
		e.ContextLimit = n
	}
	if s := strings.TrimSpace(raw["COMPACT_AT"]); s != "" {
		if strings.HasSuffix(s, "%") {
			pct, err := strconv.ParseFloat(strings.TrimSuffix(s, "%"), 64)
			switch {
			case err != nil || pct <= 0 || pct > 100:
				problems = append(problems, fmt.Sprintf("COMPACT_AT: %q is not a valid percentage", s))
			case e.ContextLimit == 0:
				problems = append(problems, "COMPACT_AT as a percentage needs CONTEXT_LIMIT")
			default:
				e.CompactAt = int(float64(e.ContextLimit) * pct / 100)
			}
		} else {
			n, err := parseTokens(s)
			if err != nil {
				problems = append(problems, "COMPACT_AT: "+err.Error())
			}
			e.CompactAt = n
		}
	}
	for key, dst := range map[string]*bool{"VISION": &e.Caps.Vision, "AUDIO": &e.Caps.Audio, "PDF": &e.Caps.PDF, "VIDEO": &e.Caps.Video} {
		if s, ok := raw[key]; ok {
			b, err := parseBool(s)
			if err != nil {
				problems = append(problems, fmt.Sprintf("%s: %v", key, err))
			}
			*dst = b
		}
	}
	if len(problems) > 0 {
		return e, errors.New(strings.Join(problems, "; "))
	}
	return e, nil
}

// parseTokens accepts 128000, 128k, 1m, 1_000_000.
func parseTokens(s string) (int, error) {
	s = strings.ToLower(strings.ReplaceAll(strings.TrimSpace(s), "_", ""))
	mult := 1
	switch {
	case strings.HasSuffix(s, "k"):
		mult, s = 1000, strings.TrimSuffix(s, "k")
	case strings.HasSuffix(s, "m"):
		mult, s = 1000000, strings.TrimSuffix(s, "m")
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f < 0 {
		return 0, fmt.Errorf("%q is not a token count (examples: 128000, 128k, 1m)", s)
	}
	return int(f * float64(mult)), nil
}

// Validate checks the .env facts.
func (e Env) Validate() []string { return e.ValidateFor(Defaults()) }

// ValidateFor checks the .env facts in the context of config.json: an API key
// is not required when provider.auth_style is "none" (local servers).
func (e Env) ValidateFor(c Config) []string {
	var p []string
	s := e.ProviderSettings(Defaults())
	if _, err := provider.Endpoint(s); err != nil {
		p = append(p, err.Error())
	}
	if e.Model == "" {
		p = append(p, "MODEL is required")
	}
	if e.APIKey == "" && c.Provider.AuthStyle != "none" {
		p = append(p, "API_KEY is required (set provider.auth_style to \"none\" for unauthenticated local servers)")
	}
	if e.CompactAt > 0 && e.ContextLimit > 0 && e.CompactAt > e.ContextLimit {
		p = append(p, "COMPACT_AT must not exceed CONTEXT_LIMIT")
	}
	return p
}

// ProviderSettings merges .env and config.json into provider settings.
func (e Env) ProviderSettings(c Config) provider.Settings {
	return provider.Settings{
		Provider: e.Provider, APIFormat: e.APIFormat, BaseURL: e.BaseURL, APIKey: e.APIKey, Model: e.Model,
		AuthStyle: c.Provider.AuthStyle, ExtraHeaders: c.Provider.ExtraHeaders, ExtraBody: c.Provider.ExtraBody,
		GeminiToolMedia: c.Provider.GeminiToolMedia, OpenAIStreamUsage: bool(c.Provider.OpenAIStreamUsage),
		OpenAIReplayReasoning: bool(c.Provider.OpenAIReplayReasoning), AnthropicVersion: c.Provider.AnthropicVersion,
		PromptCache: bool(c.Provider.PromptCache), AnthropicMaxTokens: c.Provider.AnthropicMaxTokens,
		StallTimeout: time.Duration(c.Network.StallTimeoutSeconds) * time.Second,
	}
}
