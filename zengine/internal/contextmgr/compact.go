package contextmgr

import (
	"strings"

	"github.com/ranjanlive/zpilot/zengine/internal/llm"
	"github.com/ranjanlive/zpilot/zengine/internal/session"
)

// SummaryPrompt is appended as the final user message of the summarisation
// request. It tells the model that attachments will be lost, so descriptions
// of them must be written into the summary.
const SummaryPrompt = `[info: context compaction]
The conversation has reached its context limit and is about to be compacted. Write a continuation summary that lets you resume this work in a fresh context where the conversation history, ALL attachments (images, PDFs, audio, video) and tool outputs will NOT be available. Only your summary survives.

Cover, concisely but completely:
1. Goal and constraints: what the user asked for, their preferences, and decisions already made.
2. Progress: what is done (files created or changed with their paths, commands run and what they showed) and what remains.
3. Key findings: facts, values, errors and conclusions you will need again. Quote exactly when precision matters (IDs, paths, numbers, code, error text).
4. Attachments: for every image, PDF, audio or video you were shown, describe what it contained in enough detail to keep working without seeing it again, and note its file path if known so it can be re-opened with view_file.
5. Next steps: the immediate next action or actions.

Do not call tools. Output only the summary text.`

const resumePrefix = "[info: context compacted]"

// SummaryRequest returns the messages for the summarisation call: the current
// conversation followed by the summary instruction.
func SummaryRequest(ctx Context) []llm.Message {
	msgs := append([]llm.Message(nil), ctx.Messages...)
	return append(msgs, llm.Message{Role: llm.RoleUser, Parts: []llm.Part{{Type: llm.PartText, Text: SummaryPrompt}}})
}

// IsUserPrompt reports whether m is a genuine user prompt: a user message with
// text that carries no tool results and is not a compaction resume note.
func IsUserPrompt(m llm.Message) bool {
	if m.Role != llm.RoleUser {
		return false
	}
	hasText := false
	for _, p := range m.Parts {
		if p.Type == llm.PartToolResult {
			return false
		}
		if p.Type == llm.PartText && strings.TrimSpace(p.Text) != "" {
			if strings.HasPrefix(p.Text, resumePrefix) {
				return false
			}
			hasText = true
		}
	}
	return hasText
}

// KeptPrompts returns the text of the last n user prompts, oldest first.
func KeptPrompts(msgs []llm.Message, n int) []string {
	if n <= 0 {
		return nil
	}
	var prompts []string
	for _, m := range msgs {
		if !IsUserPrompt(m) {
			continue
		}
		var sb strings.Builder
		for _, p := range m.Parts {
			if p.Type == llm.PartText {
				if sb.Len() > 0 {
					sb.WriteString("\n")
				}
				sb.WriteString(p.Text)
			}
		}
		prompts = append(prompts, sb.String())
	}
	if len(prompts) > n {
		prompts = prompts[len(prompts)-n:]
	}
	return prompts
}

// NewCompaction assembles the compaction record for a finished summary.
func NewCompaction(ctx Context, summary, reason string, keep, tokensBefore, bytesBefore int) session.Compaction {
	seen := map[string]bool{}
	var paths []string
	for _, a := range ctx.Attachments {
		label := a.Path
		if label == "" {
			label = a.Name
		}
		if label != "" && !seen[label] {
			seen[label] = true
			paths = append(paths, label)
		}
	}
	return session.Compaction{
		Summary:      strings.TrimSpace(summary),
		KeptPrompts:  KeptPrompts(ctx.Messages, keep),
		Reason:       reason,
		TokensBefore: tokensBefore,
		BytesBefore:  bytesBefore,
		RemovedBlobs: len(BlobIDs(ctx.Attachments)),
		RemovedPaths: paths,
	}
}
