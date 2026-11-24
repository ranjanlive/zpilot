// Package model holds ZPilot's domain types and their validation rules.
// It has no dependencies on storage, transport or the engine, so every other
// package can share these definitions.
package model

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/ranjanlive/zpilot/internal/trigger"
)

// ---------------------------------------------------------------------------
// Security levels
// ---------------------------------------------------------------------------

// Level is how much an agent may do on the machine. Levels are enforced by the
// engine configuration that ZPilot writes for each run, never by prompts.
type Level string

const (
	// LevelRestricted: read files in the workspace only. No writes, no commands.
	LevelRestricted Level = "restricted"
	// LevelStandard: read and write files in the workspace. No commands.
	LevelStandard Level = "standard"
	// LevelTrusted: standard plus shell commands (run in the workspace, with
	// the daemon user's OS permissions).
	LevelTrusted Level = "trusted"
	// LevelFull: trusted plus file access outside the workspace.
	LevelFull Level = "full"
)

// Levels lists the levels from least to most privileged.
var Levels = []Level{LevelRestricted, LevelStandard, LevelTrusted, LevelFull}

func (l Level) Valid() bool { return slices.Contains(Levels, l) }

// Rank orders levels: restricted 0 .. full 3. Unknown levels rank -1.
func (l Level) Rank() int { return slices.Index(Levels, l) }

// RunsCommands reports whether the level allows shell commands.
func (l Level) RunsCommands() bool { return l.Rank() >= LevelTrusted.Rank() }

// Describe is a one-line, user-facing explanation of the level.
func (l Level) Describe() string {
	switch l {
	case LevelRestricted:
		return "Read files in its workspace. Cannot change files or run commands."
	case LevelStandard:
		return "Read and write files in its workspace. Cannot run commands."
	case LevelTrusted:
		return "Workspace files plus shell commands with your user's permissions."
	case LevelFull:
		return "Shell commands and file access anywhere your user can reach."
	}
	return "unknown level"
}

// ---------------------------------------------------------------------------
// Identifiers
// ---------------------------------------------------------------------------

var (
	usernameRe = regexp.MustCompile(`^[a-z][a-z0-9_]{1,31}$`)
	slugRe     = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)
)

// Reserved usernames cannot be taken by agents: they name the human owner and
// system sources in message envelopes.
var reservedUsernames = []string{"user", "owner", "admin", "system", "all", "everyone", "here", "zpilot", "zauto", "zautomation", "schedule", "channel", "webhook"}

// ManagerUsername is the built-in agent that administers the installation.
const ManagerUsername = "manager"

// NormalizeUsername strips a leading @ and lowercases.
func NormalizeUsername(s string) string {
	return strings.ToLower(strings.TrimPrefix(strings.TrimSpace(s), "@"))
}

// ValidateUsername checks an agent handle (without the @).
func ValidateUsername(u string) error {
	if !usernameRe.MatchString(u) {
		return fmt.Errorf("username %q must be 2-32 characters: a lowercase letter first, then lowercase letters, digits or _", u)
	}
	if slices.Contains(reservedUsernames, u) {
		return fmt.Errorf("username %q is reserved", u)
	}
	return nil
}

// ValidSlug reports whether s is a safe library name (tools, skills).
func ValidSlug(s string) bool { return slugRe.MatchString(s) }

// NewID returns a sortable, prefixed random identifier such as
// "msg_20261124T093011_9f3c2a1b".
func NewID(prefix string) string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return prefix + "_" + time.Now().UTC().Format("20060102T150405") + "_" + hex.EncodeToString(b[:])
}

// NewToken returns 32 random bytes as hex.
func NewToken() string {
	var b [32]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// ---------------------------------------------------------------------------
// Agents
// ---------------------------------------------------------------------------

// Agent is a user-defined agent profile. Each run materialises it into an
// engine home directory (see package profile).
type Agent struct {
	ID          string `json:"id"`
	Username    string `json:"username"`
	Name        string `json:"name"`
	Icon        string `json:"icon"`        // an emoji or short text
	Color       string `json:"color"`       // accent colour for the UI, #rrggbb
	Description string `json:"description"` // shown to other agents in the directory
	Instruction string `json:"instruction"` // system instruction

	ModelID   string `json:"model_id"`
	Level     Level  `json:"level"`
	Workspace string `json:"workspace"` // absolute; empty = <data>/workspaces/<username>

	Skills []string `json:"skills"` // skill library names
	Tools  []string `json:"tools"`  // tool library ids

	// Contacts are the agents this agent may message ("*" = any).
	Contacts []string `json:"contacts"`
	// AcceptFrom limits which agents may message this one ("*" = any, the
	// default). The human owner can always message every agent.
	AcceptFrom []string `json:"accept_from"`

	MaxParallel    int `json:"max_parallel"`    // concurrent threads (default 2)
	TimeoutMinutes int `json:"timeout_minutes"` // per run (default 30)
	MaxToolRounds  int `json:"max_tool_rounds"` // per run (default 200)

	// ExternalInputWithCommands must be set to attach a channel to an agent
	// that can run commands: messages from Telegram, webhooks and so on are
	// untrusted and could try to make the agent run something harmful.
	ExternalInputWithCommands bool `json:"external_input_with_commands"`
	// ApproveCommands makes every shell command (and every external tool that
	// asks for permission) wait for the owner's approval.
	ApproveCommands bool `json:"approve_commands"`

	// Resource limits for each run (0 = unlimited). MemoryMB applies on
	// Windows (Job Object) and Linux (address-space limit); MaxProcesses on
	// Windows. Isolate (Linux, needs bubblewrap) makes the whole file system
	// read-only for the agent except its workspace, home and /tmp.
	MemoryMB     int  `json:"memory_mb"`
	MaxProcesses int  `json:"max_processes"`
	Isolate      bool `json:"isolate"`

	Builtin  bool      `json:"builtin,omitempty"`
	Disabled bool      `json:"disabled"`
	Created  time.Time `json:"created"`
	Updated  time.Time `json:"updated"`
}

// Defaults fills zero values.
func (a *Agent) Defaults() {
	a.Username = NormalizeUsername(a.Username)
	if a.Level == "" {
		a.Level = LevelStandard
	}
	if a.Icon == "" {
		a.Icon = "🤖"
	}
	if a.Color == "" {
		a.Color = "#6366f1"
	}
	if a.MaxParallel <= 0 {
		a.MaxParallel = 2
	}
	if a.TimeoutMinutes <= 0 {
		a.TimeoutMinutes = 30
	}
	if a.MaxToolRounds <= 0 {
		a.MaxToolRounds = 200
	}
	if a.AcceptFrom == nil {
		a.AcceptFrom = []string{"*"}
	}
	if a.Contacts == nil {
		a.Contacts = []string{}
	}
	if a.Skills == nil {
		a.Skills = []string{}
	}
	if a.Tools == nil {
		a.Tools = []string{}
	}
	a.Contacts = normalizeHandles(a.Contacts)
	a.AcceptFrom = normalizeHandles(a.AcceptFrom)
}

func normalizeHandles(in []string) []string {
	out := []string{}
	for _, h := range in {
		h = NormalizeUsername(h)
		if h != "" && !slices.Contains(out, h) {
			out = append(out, h)
		}
	}
	return out
}

// Validate checks the profile on its own. Cross-references (model, tools,
// contacts) are checked by the service, which knows what exists.
func (a *Agent) Validate() error {
	if err := ValidateUsername(a.Username); err != nil && !(a.Builtin && a.Username == ManagerUsername) {
		return err
	}
	if strings.TrimSpace(a.Name) == "" {
		return fmt.Errorf("name is required")
	}
	if len(a.Name) > 64 {
		return fmt.Errorf("name is too long (max 64 characters)")
	}
	if len([]rune(a.Icon)) > 8 {
		return fmt.Errorf("icon should be an emoji or up to 8 characters")
	}
	if a.Color != "" && !regexp.MustCompile(`^#[0-9a-fA-F]{6}$`).MatchString(a.Color) {
		return fmt.Errorf("color must look like #6366f1")
	}
	if !a.Level.Valid() {
		return fmt.Errorf("level must be one of restricted, standard, trusted, full")
	}
	if a.Workspace != "" && !filepath.IsAbs(a.Workspace) {
		return fmt.Errorf("workspace must be an absolute path")
	}
	if len(a.Instruction) > 64*1024 {
		return fmt.Errorf("instruction is too long (max 64 KB)")
	}
	for _, s := range a.Skills {
		if !ValidSlug(s) {
			return fmt.Errorf("invalid skill name %q", s)
		}
	}
	for _, t := range a.Tools {
		if !ValidSlug(t) {
			return fmt.Errorf("invalid tool id %q", t)
		}
	}
	for _, list := range [][]string{a.Contacts, a.AcceptFrom} {
		for _, h := range list {
			if h != "*" && ValidateUsername(h) != nil && h != ManagerUsername {
				return fmt.Errorf("invalid agent handle @%s", h)
			}
		}
	}
	if a.MaxParallel > 16 {
		return fmt.Errorf("max_parallel must be 1-16")
	}
	if a.MemoryMB < 0 || a.MemoryMB > 1<<20 || a.MaxProcesses < 0 || a.MaxProcesses > 10000 {
		return fmt.Errorf("resource limits are out of range")
	}
	if a.MemoryMB > 0 && a.MemoryMB < 128 {
		return fmt.Errorf("memory limit must be at least 128 MB (the engine itself needs memory)")
	}
	if a.TimeoutMinutes > 24*60 {
		return fmt.Errorf("timeout_minutes must be at most 1440")
	}
	return nil
}

// MayContact reports whether this agent's contact list includes target.
func (a *Agent) MayContact(target string) bool {
	return slices.Contains(a.Contacts, "*") || slices.Contains(a.Contacts, target)
}

// Accepts reports whether this agent accepts messages from the agent sender.
func (a *Agent) Accepts(sender string) bool {
	return slices.Contains(a.AcceptFrom, "*") || slices.Contains(a.AcceptFrom, sender)
}

// ---------------------------------------------------------------------------
// Models
// ---------------------------------------------------------------------------

// ModelProfile is a reusable provider configuration. The API key is stored in
// the vault under KeyRef and passed to the engine through its environment.
type ModelProfile struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Provider     string `json:"provider"` // gemini | openai_compatible | anthropic | custom
	APIFormat    string `json:"api_format,omitempty"`
	BaseURL      string `json:"base_url"`
	Model        string `json:"model"`
	ContextLimit int    `json:"context_limit,omitempty"`
	CompactAt    string `json:"compact_at,omitempty"`
	Vision       bool   `json:"vision"`
	Audio        bool   `json:"audio"`
	PDF          bool   `json:"pdf"`
	Video        bool   `json:"video"`
	AuthStyle    string `json:"auth_style,omitempty"` // "" | bearer | x-api-key | x-goog-api-key | none
	KeyRef       string `json:"key_ref,omitempty"`
	HasKey       bool   `json:"has_key"` // derived, for the UI

	Created time.Time `json:"created"`
	Updated time.Time `json:"updated"`
}

var providers = []string{"gemini", "openai_compatible", "anthropic", "custom"}

func (m *ModelProfile) Validate() error {
	if strings.TrimSpace(m.Name) == "" {
		return fmt.Errorf("name is required")
	}
	if !slices.Contains(providers, m.Provider) {
		return fmt.Errorf("provider must be one of %s", strings.Join(providers, ", "))
	}
	if m.Provider == "custom" {
		if m.BaseURL == "" {
			return fmt.Errorf("custom providers need the exact endpoint URL in base_url")
		}
		if !slices.Contains([]string{"gemini", "openai", "anthropic"}, m.APIFormat) {
			return fmt.Errorf("custom providers need api_format: gemini, openai or anthropic")
		}
	}
	if m.Provider == "openai_compatible" && m.BaseURL == "" {
		return fmt.Errorf("openai_compatible providers need base_url")
	}
	if m.BaseURL != "" && !strings.HasPrefix(m.BaseURL, "https://") && !strings.HasPrefix(m.BaseURL, "http://") {
		return fmt.Errorf("base_url must start with http:// or https://")
	}
	if strings.TrimSpace(m.Model) == "" {
		return fmt.Errorf("model is required")
	}
	if !slices.Contains([]string{"", "bearer", "x-api-key", "x-goog-api-key", "none"}, m.AuthStyle) {
		return fmt.Errorf("auth_style must be bearer, x-api-key, x-goog-api-key or none")
	}
	return nil
}

// ---------------------------------------------------------------------------
// Threads and messages
// ---------------------------------------------------------------------------

// Sender and recipient addresses in messages.
const (
	AddrUser = "@user" // the human owner (web UI, CLI)
)

// AgentAddr returns "@username".
func AgentAddr(username string) string { return "@" + username }

// IsAgentAddr reports whether addr names an agent (not the owner or a system source).
func IsAgentAddr(addr string) bool { return strings.HasPrefix(addr, "@") && addr != AddrUser }

// Thread is one conversation. Every agent taking part keeps its own engine
// session for the thread, named SessionName().
type Thread struct {
	ID         string    `json:"id"`
	Title      string    `json:"title"`
	Origin     string    `json:"origin"` // user | channel:<id> | schedule:<id> | webhook:<id>
	ChannelRef string    `json:"channel_ref,omitempty"`
	Agents     []string  `json:"agents"` // usernames that took part
	Runs       int       `json:"runs"`
	MaxRuns    int       `json:"max_runs"`
	Archived   bool      `json:"archived"`
	Created    time.Time `json:"created"`
	Updated    time.Time `json:"updated"`
	LastText   string    `json:"last_text,omitempty"`
}

// SessionName is the engine session used by every agent in this thread.
func (t *Thread) SessionName() string { return "th-" + t.ID }

// ThreadFromSession reverses SessionName.
func ThreadFromSession(s string) (string, bool) {
	return strings.CutPrefix(s, "th-")
}

// Message kinds.
const (
	KindRequest = "request" // asks the recipient to act
	KindReply   = "reply"   // the recipient's answer
	KindError   = "error"   // a run failed; text explains
	KindNotice  = "notice"  // informational (budget reached, rejected, ...)
)

// Message statuses (for requests to agents).
const (
	StatusQueued   = "queued"
	StatusRunning  = "running"
	StatusDone     = "done"
	StatusFailed   = "failed"
	StatusRejected = "rejected"
	StatusCanceled = "canceled"
)

// Delivery modes for agent-to-agent requests.
const (
	ModeAsk  = "ask"  // the sender waits for the reply inside its run
	ModeTell = "tell" // the reply arrives later as a new turn for the sender
)

// Message is one entry in a thread.
type Message struct {
	ID       string `json:"id"`
	ThreadID string `json:"thread_id"`
	From     string `json:"from"` // @user, @agent, channel:<id>:<chat>, schedule:<id>, webhook:<id>
	FromName string `json:"from_name,omitempty"`
	To       string `json:"to"` // @agent or the original sender for replies
	Kind     string `json:"kind"`
	Text     string `json:"text"`
	Mode     string `json:"mode,omitempty"`
	ReplyTo  string `json:"reply_to,omitempty"`
	// Chain lists the agents waiting (synchronously) on this request, oldest
	// first. It is how cycles (A asks B asks A) are refused before they deadlock.
	Chain  []string `json:"chain,omitempty"`
	Hop    int      `json:"hop"`
	Status string   `json:"status,omitempty"`
	RunID  string   `json:"run_id,omitempty"`
	// Detail carries a machine-readable reason for rejected/failed requests.
	Detail  string    `json:"detail,omitempty"`
	Created time.Time `json:"created"`
	Updated time.Time `json:"updated,omitzero"`
}

// Pending reports whether a request still waits to be (or is being) handled.
func (m *Message) Pending() bool {
	return m.Kind == KindRequest && (m.Status == StatusQueued || m.Status == StatusRunning)
}

// ---------------------------------------------------------------------------
// Runs
// ---------------------------------------------------------------------------

// Run states.
const (
	RunRunning     = "running"
	RunDone        = "done"
	RunFailed      = "failed"
	RunTimeout     = "timeout"
	RunStopped     = "stopped"
	RunInterrupted = "interrupted" // the daemon stopped while it ran
)

// Run is one engine invocation that handles one request message.
type Run struct {
	ID        string    `json:"id"`
	AgentID   string    `json:"agent_id"`
	Username  string    `json:"username"`
	ThreadID  string    `json:"thread_id"`
	MessageID string    `json:"message_id"`
	State     string    `json:"state"`
	ExitCode  int       `json:"exit_code"`
	Error     string    `json:"error,omitempty"`
	Prompt    int       `json:"prompt_tokens"`
	Answer    int       `json:"answer_tokens"`
	Requests  int       `json:"requests"`
	Started   time.Time `json:"started"`
	Ended     time.Time `json:"ended,omitzero"`
}

// ---------------------------------------------------------------------------
// Schedules
// ---------------------------------------------------------------------------

// Schedule sends Task to an agent whenever Trigger fires.
type Schedule struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	AgentID string `json:"agent_id"`
	// WorkflowID, when set, runs a workflow (with Task as its input)
	// instead of messaging AgentID.
	WorkflowID string          `json:"workflow_id,omitempty"`
	Task       string          `json:"task"`
	Trigger    trigger.Trigger `json:"trigger"`
	Enabled    bool            `json:"enabled"`
	// FreshThread starts a new conversation each time; otherwise runs share
	// one thread (and the agent remembers earlier runs).
	FreshThread bool      `json:"fresh_thread"`
	ThreadID    string    `json:"thread_id,omitempty"`
	NextRun     time.Time `json:"next_run,omitzero"`
	LastRun     time.Time `json:"last_run,omitzero"`
	Created     time.Time `json:"created"`
	Updated     time.Time `json:"updated"`
}

func (s *Schedule) Validate() error {
	if strings.TrimSpace(s.Name) == "" {
		return fmt.Errorf("name is required")
	}
	if strings.TrimSpace(s.Task) == "" && s.WorkflowID == "" {
		return fmt.Errorf("task is required")
	}
	if s.AgentID == "" && s.WorkflowID == "" {
		return fmt.Errorf("choose an agent or a workflow")
	}
	return s.Trigger.Validate()
}

// ---------------------------------------------------------------------------
// Channels
// ---------------------------------------------------------------------------

// Channel kinds.
const (
	ChannelTelegram = "telegram"
	ChannelWebhook  = "webhook"
	ChannelDiscord  = "discord"
	ChannelEmail    = "email"
	ChannelWhatsApp = "whatsapp"
)

// ChannelKinds lists the supported channel kinds.
var ChannelKinds = []string{ChannelTelegram, ChannelDiscord, ChannelEmail, ChannelWhatsApp, ChannelWebhook}

// channelSettings lists the non-secret settings each kind requires.
var channelSettings = map[string][]string{
	ChannelEmail:    {"address", "imap_host", "smtp_host"},
	ChannelWhatsApp: {"phone_number_id", "verify_token"},
}

// Channel connects an external messaging service to one agent.
type Channel struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	Name    string `json:"name"`
	AgentID string `json:"agent_id"`
	Enabled bool   `json:"enabled"`
	// SecretRef points at the main secret: bot token (telegram, discord),
	// shared secret (webhook), password (email) or access token (whatsapp).
	SecretRef string `json:"secret_ref,omitempty"`
	// Secret2Ref is a second secret where a kind needs one (whatsapp app secret).
	Secret2Ref string `json:"secret2_ref,omitempty"`
	// Settings are non-secret, kind-specific options (hosts, ids, flags).
	Settings map[string]string `json:"settings,omitempty"`
	// Allowed lists who may talk to the agent (telegram chat ids). Empty means
	// nobody: unknown senders are recorded in Rejected so the owner can allow them.
	Allowed  []string  `json:"allowed"`
	Rejected []Sender  `json:"rejected,omitempty"`
	Offset   int64     `json:"offset,omitempty"` // telegram update offset
	Status   string    `json:"status,omitempty"` // runtime status for the UI
	Created  time.Time `json:"created"`
	Updated  time.Time `json:"updated"`
}

// Sender is an external identity that tried to reach an agent.
type Sender struct {
	ID   string    `json:"id"`
	Name string    `json:"name"`
	Last time.Time `json:"last"`
	Text string    `json:"text,omitempty"`
}

func (c *Channel) Validate() error {
	if !slices.Contains(ChannelKinds, c.Kind) {
		return fmt.Errorf("kind must be one of %s", strings.Join(ChannelKinds, ", "))
	}
	for _, k := range channelSettings[c.Kind] {
		if strings.TrimSpace(c.Settings[k]) == "" {
			return fmt.Errorf("%s channels need the setting %q", c.Kind, k)
		}
	}
	for k, v := range c.Settings {
		if len(k) > 64 || len(v) > 512 {
			return fmt.Errorf("setting %q is too long", k)
		}
	}
	if strings.TrimSpace(c.Name) == "" {
		return fmt.Errorf("name is required")
	}
	if c.AgentID == "" {
		return fmt.Errorf("agent is required")
	}
	for _, a := range c.Allowed {
		if strings.TrimSpace(a) == "" {
			return fmt.Errorf("allowed ids must not be empty")
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Proposals (changes requested by the manager agent)
// ---------------------------------------------------------------------------

// Proposal actions.
const (
	ActCreateAgent    = "create_agent"
	ActUpdateAgent    = "update_agent"
	ActDeleteAgent    = "delete_agent"
	ActCreateSchedule = "create_schedule"
	ActDeleteSchedule = "delete_schedule"
	ActInstallTool    = "install_tool"
)

// ProposalActions lists what the manager may propose.
var ProposalActions = []string{ActCreateAgent, ActUpdateAgent, ActDeleteAgent, ActCreateSchedule, ActDeleteSchedule, ActInstallTool}

// Proposal states.
const (
	ProposalPending  = "pending"
	ProposalApproved = "approved"
	ProposalRejected = "rejected"
	ProposalFailed   = "failed"
)

// Proposal is a change an agent asked for. Nothing happens until the owner
// approves it.
type Proposal struct {
	ID       string          `json:"id"`
	By       string          `json:"by"` // agent username
	ThreadID string          `json:"thread_id,omitempty"`
	Action   string          `json:"action"`
	Target   string          `json:"target,omitempty"` // e.g. the agent username
	Payload  json.RawMessage `json:"payload"`
	Reason   string          `json:"reason"`
	State    string          `json:"state"`
	Result   string          `json:"result,omitempty"`
	Created  time.Time       `json:"created"`
	Decided  time.Time       `json:"decided,omitzero"`
}

// ---------------------------------------------------------------------------
// Command approvals
// ---------------------------------------------------------------------------

// Approval states.
const (
	ApprovalPending = "pending"
	ApprovalAllowed = "allowed"
	ApprovalDenied  = "denied"
	ApprovalExpired = "expired"
)

// Approval is a tool call (usually a shell command) waiting for the owner.
type Approval struct {
	ID       string    `json:"id"`
	RunID    string    `json:"run_id"`
	AgentID  string    `json:"agent_id"`
	Username string    `json:"username"`
	ThreadID string    `json:"thread_id"`
	Tool     string    `json:"tool"`
	Action   string    `json:"action"`
	Detail   string    `json:"detail"`
	State    string    `json:"state"`
	Reason   string    `json:"reason,omitempty"`
	By       string    `json:"by,omitempty"`
	Created  time.Time `json:"created"`
	Decided  time.Time `json:"decided,omitzero"`
}

// ---------------------------------------------------------------------------
// Workflows
// ---------------------------------------------------------------------------

// WorkflowStep is one node of a workflow graph: a prompt sent to an agent.
// Prompt may use {{input}} and {{steps.<id>.output}} of upstream steps.
type WorkflowStep struct {
	ID        string   `json:"id"`
	Agent     string   `json:"agent"` // username
	Prompt    string   `json:"prompt"`
	DependsOn []string `json:"depends_on"`
	// When, if set, runs the step only if an upstream output contains (or,
	// with Not, does not contain) the given text (case-insensitive).
	When *StepCondition `json:"when,omitempty"`
	X    float64        `json:"x"`
	Y    float64        `json:"y"`
}

// StepCondition gates a step on an upstream output.
type StepCondition struct {
	Step     string `json:"step"`
	Contains string `json:"contains"`
	Not      bool   `json:"not"`
}

// Workflow is a reusable graph of agent steps.
type Workflow struct {
	ID          string         `json:"id"`
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Steps       []WorkflowStep `json:"steps"`
	Created     time.Time      `json:"created"`
	Updated     time.Time      `json:"updated"`
}

// Workflow run and step states.
const (
	WFRunning  = "running"
	WFDone     = "done"
	WFFailed   = "failed"
	WFCanceled = "canceled"
	WFPending  = "pending"
	WFSkipped  = "skipped"
)

// StepRun is the state of one step in a workflow run.
type StepRun struct {
	State     string    `json:"state"`
	MessageID string    `json:"message_id,omitempty"`
	Output    string    `json:"output,omitempty"`
	Error     string    `json:"error,omitempty"`
	Started   time.Time `json:"started,omitzero"`
	Ended     time.Time `json:"ended,omitzero"`
}

// WorkflowRun is one execution of a workflow.
type WorkflowRun struct {
	ID         string              `json:"id"`
	WorkflowID string              `json:"workflow_id"`
	Name       string              `json:"name"`
	Input      string              `json:"input"`
	ThreadID   string              `json:"thread_id"`
	State      string              `json:"state"`
	Steps      map[string]*StepRun `json:"steps"`
	Graph      []WorkflowStep      `json:"graph"` // the definition as it was when the run started
	By         string              `json:"by"`
	Started    time.Time           `json:"started"`
	Ended      time.Time           `json:"ended,omitzero"`
}

var stepIDRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)

// Validate checks names, references and that the graph has no cycles.
func (w *Workflow) Validate() error {
	if strings.TrimSpace(w.Name) == "" {
		return fmt.Errorf("name is required")
	}
	if len(w.Steps) == 0 {
		return fmt.Errorf("add at least one step")
	}
	if len(w.Steps) > 50 {
		return fmt.Errorf("a workflow can have at most 50 steps")
	}
	ids := map[string]bool{}
	for _, s := range w.Steps {
		if !stepIDRe.MatchString(s.ID) {
			return fmt.Errorf("step id %q must be lowercase letters, digits or _", s.ID)
		}
		if ids[s.ID] {
			return fmt.Errorf("duplicate step id %q", s.ID)
		}
		ids[s.ID] = true
		if strings.TrimSpace(s.Prompt) == "" {
			return fmt.Errorf("step %s has no prompt", s.ID)
		}
		if NormalizeUsername(s.Agent) == "" {
			return fmt.Errorf("step %s has no agent", s.ID)
		}
	}
	deps := map[string][]string{}
	for _, s := range w.Steps {
		for _, d := range s.DependsOn {
			if !ids[d] {
				return fmt.Errorf("step %s depends on unknown step %q", s.ID, d)
			}
		}
		deps[s.ID] = s.DependsOn
		for _, ref := range StepRefs(s.Prompt) {
			if !ids[ref] {
				return fmt.Errorf("step %s uses {{steps.%s.output}} but there is no step %q", s.ID, ref, ref)
			}
			if !Upstream(deps, s.ID, ref) && !slices.Contains(s.DependsOn, ref) {
				return fmt.Errorf("step %s uses the output of %s, so it must depend on it (directly or indirectly)", s.ID, ref)
			}
		}
		if s.When != nil && !ids[s.When.Step] {
			return fmt.Errorf("step %s has a condition on unknown step %q", s.ID, s.When.Step)
		}
	}
	// cycle check (Kahn)
	indeg := map[string]int{}
	for _, s := range w.Steps {
		indeg[s.ID] += 0
		for range s.DependsOn {
			indeg[s.ID]++
		}
	}
	queue := []string{}
	for id, n := range indeg {
		if n == 0 {
			queue = append(queue, id)
		}
	}
	seen := 0
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		seen++
		for _, s := range w.Steps {
			if slices.Contains(s.DependsOn, id) {
				indeg[s.ID]--
				if indeg[s.ID] == 0 {
					queue = append(queue, s.ID)
				}
			}
		}
	}
	if seen != len(w.Steps) {
		return fmt.Errorf("the steps form a cycle; a workflow must flow in one direction")
	}
	for _, s := range w.Steps {
		if s.When != nil && !Upstream(deps, s.ID, s.When.Step) {
			return fmt.Errorf("step %s has a condition on %s, so it must depend on it", s.ID, s.When.Step)
		}
	}
	return nil
}

var stepRefRe = regexp.MustCompile(`\{\{\s*steps\.([a-z0-9_]+)\.output\s*\}\}`)

// StepRefs lists the step ids a prompt template refers to.
func StepRefs(prompt string) []string {
	var out []string
	for _, m := range stepRefRe.FindAllStringSubmatch(prompt, -1) {
		if !slices.Contains(out, m[1]) {
			out = append(out, m[1])
		}
	}
	return out
}

// Upstream reports whether target is reachable from id by following dependencies.
func Upstream(deps map[string][]string, id, target string) bool {
	seen := map[string]bool{}
	var walk func(string) bool
	walk = func(n string) bool {
		for _, d := range deps[n] {
			if d == target {
				return true
			}
			if !seen[d] {
				seen[d] = true
				if walk(d) {
					return true
				}
			}
		}
		return false
	}
	return walk(id)
}

// RenderPrompt fills {{input}} and {{steps.<id>.output}}.
func RenderPrompt(tpl, input string, outputs map[string]string) string {
	out := strings.ReplaceAll(tpl, "{{input}}", input)
	out = strings.ReplaceAll(out, "{{ input }}", input)
	return stepRefRe.ReplaceAllStringFunc(out, func(m string) string {
		id := stepRefRe.FindStringSubmatch(m)[1]
		return outputs[id]
	})
}

// ---------------------------------------------------------------------------
// Users
// ---------------------------------------------------------------------------

// Roles, from least to most privileged.
const (
	RoleViewer   = "viewer"   // read conversations, agents and activity
	RoleOperator = "operator" // also talk to agents, run schedules, stop runs
	RoleAdmin    = "admin"    // everything: configuration, approvals, users
)

// Roles lists the roles in increasing privilege.
var Roles = []string{RoleViewer, RoleOperator, RoleAdmin}

// RoleRank orders roles; unknown roles rank -1.
func RoleRank(r string) int { return slices.Index(Roles, r) }

// User is a dashboard account.
type User struct {
	ID           string    `json:"id"`
	Username     string    `json:"username"`
	Name         string    `json:"name"`
	Role         string    `json:"role"`
	PasswordHash string    `json:"password_hash,omitempty"`
	Disabled     bool      `json:"disabled"`
	Created      time.Time `json:"created"`
	LastLogin    time.Time `json:"last_login,omitzero"`
}

var userRe = regexp.MustCompile(`^[a-z][a-z0-9_.-]{1,31}$`)

// Validate checks a user record (not the password).
func (u *User) Validate() error {
	if !userRe.MatchString(u.Username) {
		return fmt.Errorf("username must be 2-32 characters: lowercase letters, digits, . _ -")
	}
	if RoleRank(u.Role) < 0 {
		return fmt.Errorf("role must be viewer, operator or admin")
	}
	if len(u.Name) > 64 {
		return fmt.Errorf("name is too long")
	}
	return nil
}

// ---------------------------------------------------------------------------
// Audit
// ---------------------------------------------------------------------------

// AuditEntry records a change to configuration or a security-relevant event.
type AuditEntry struct {
	Time   time.Time `json:"time"`
	Actor  string    `json:"actor"`
	Action string    `json:"action"`
	Target string    `json:"target,omitempty"`
	Detail string    `json:"detail,omitempty"`
}

// ---------------------------------------------------------------------------
// Settings
// ---------------------------------------------------------------------------

// Settings are installation-wide limits.
type Settings struct {
	GlobalParallel int `json:"global_parallel"` // concurrent runs (default 4)
	MaxHops        int `json:"max_hops"`        // agent-to-agent depth (default 6)
	ThreadMaxRuns  int `json:"thread_max_runs"` // runs per thread (default 40)
	AskTimeoutMin  int `json:"ask_timeout_minutes"`
	// ApprovalTimeoutMin denies a pending command approval after this long.
	ApprovalTimeoutMin int `json:"approval_timeout_minutes"`
	// DefaultModelID is used for new agents and the manager.
	DefaultModelID string `json:"default_model_id"`
}

func (s *Settings) Defaults() {
	if s.GlobalParallel <= 0 {
		s.GlobalParallel = 4
	}
	if s.MaxHops <= 0 {
		s.MaxHops = 6
	}
	if s.ThreadMaxRuns <= 0 {
		s.ThreadMaxRuns = 40
	}
	if s.AskTimeoutMin <= 0 {
		s.AskTimeoutMin = 30
	}
	if s.ApprovalTimeoutMin <= 0 {
		s.ApprovalTimeoutMin = 30
	}
}

func (s *Settings) Validate() error {
	if s.GlobalParallel > 64 {
		return fmt.Errorf("global_parallel must be 1-64")
	}
	if s.MaxHops > 20 {
		return fmt.Errorf("max_hops must be 1-20")
	}
	if s.ThreadMaxRuns > 1000 {
		return fmt.Errorf("thread_max_runs must be 1-1000")
	}
	if s.AskTimeoutMin > 24*60 {
		return fmt.Errorf("ask_timeout_minutes must be at most 1440")
	}
	return nil
}
