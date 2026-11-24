package contextmgr

import (
	"github.com/ranjanlive/zpilot/zengine/internal/llm"
)

// Base64 inflates payloads by 4/3 on the wire.
func wireSize(n int64) int64 { return (n*4 + 2) / 3 }

// Policy is the attachment and size policy from config.json.
type Policy struct {
	// MaxTurnToKeepAttachment prunes attachments older than N user turns (0 = never by age).
	MaxTurnToKeepAttachment int
	// PruneBatchBytes defers age-based pruning until this many wire bytes have
	// expired, so the cached request prefix changes rarely.
	PruneBatchBytes int64
	// BodyLimit is the largest acceptable request body in bytes.
	BodyLimit          int
	AutocompactOnLimit bool
	RemoveOldOnLimit   bool
}

// ExpiredByAge returns the attachments that exceed MaxTurnToKeepAttachment.
// Pruning is deferred (nil) until the expired payload reaches the batch size,
// unless force is set.
func ExpiredByAge(ctx Context, p Policy, force bool) []Attachment {
	if p.MaxTurnToKeepAttachment <= 0 {
		return nil
	}
	cutoff := ctx.Turn - p.MaxTurnToKeepAttachment // turns <= cutoff are expired
	var expired []Attachment
	var total int64
	for _, a := range ctx.Attachments {
		if a.TurnNo <= cutoff {
			expired = append(expired, a)
			total += wireSize(a.Size)
		}
	}
	if len(expired) == 0 || (!force && total < p.PruneBatchBytes) {
		return nil
	}
	return expired
}

// LimitPlan is the outcome of fitting a request under the body limit.
type LimitPlan struct {
	// Prune lists attachments to remove, oldest first.
	Prune []Attachment
	// NeedCompact is true when removing every attachment would still not fit.
	NeedCompact bool
	// EstimatedAfter is the estimated body size after pruning.
	EstimatedAfter int
}

// PlanForLimit decides how to bring a request of bodyBytes under the limit.
// Removing old attachments is preferred when enabled; compaction is the
// fallback (or the only option when removal is disabled).
func PlanForLimit(ctx Context, bodyBytes int, p Policy) LimitPlan {
	plan := LimitPlan{EstimatedAfter: bodyBytes}
	if p.BodyLimit <= 0 || bodyBytes <= p.BodyLimit {
		return plan
	}
	if !p.RemoveOldOnLimit {
		plan.NeedCompact = p.AutocompactOnLimit
		return plan
	}
	// Group occurrences by blob: removing a blob removes every copy of it.
	type group struct {
		first Attachment
		bytes int64
	}
	var order []string
	groups := map[string]*group{}
	for _, a := range ctx.Attachments {
		g := groups[a.Blob]
		if g == nil {
			g = &group{first: a}
			groups[a.Blob] = g
			order = append(order, a.Blob)
		}
		g.bytes += wireSize(a.Size)
	}
	est := int64(bodyBytes)
	for _, id := range order {
		if est <= int64(p.BodyLimit) {
			break
		}
		g := groups[id]
		plan.Prune = append(plan.Prune, g.first)
		est -= g.bytes
	}
	plan.EstimatedAfter = int(est)
	if est > int64(p.BodyLimit) {
		// Even with every attachment gone the text alone is too big.
		plan.Prune = nil
		plan.NeedCompact = p.AutocompactOnLimit
		plan.EstimatedAfter = bodyBytes
	}
	return plan
}

// BlobIDs returns the distinct blob IDs of the attachments.
func BlobIDs(as []Attachment) []string {
	seen := map[string]bool{}
	var out []string
	for _, a := range as {
		if !seen[a.Blob] {
			seen[a.Blob] = true
			out = append(out, a.Blob)
		}
	}
	return out
}

// ---- token estimation ----

// EstimateMediaTokens is a coarse guess used only when the provider reports no
// usage. It is deliberately simple and labelled as an estimate everywhere.
func EstimateMediaTokens(m llm.Media) int {
	if m.Blob == "" {
		return 0
	}
	switch m.Kind {
	case llm.MediaImage:
		return 1000
	case llm.MediaPDF:
		return max(500, int(m.Size/200))
	case llm.MediaAudio, llm.MediaVideo:
		return max(300, int(m.Size/1000))
	}
	return 0
}

// EstimateTokens approximates the prompt size of a request: text bytes / 4
// plus a per-attachment guess.
func EstimateTokens(system string, msgs []llm.Message, tools []llm.ToolSpec) int {
	bytes := len(system)
	media := 0
	for _, t := range tools {
		bytes += len(t.Name) + len(t.Description) + len(t.Schema)
	}
	for _, m := range msgs {
		for _, p := range m.Parts {
			bytes += len(p.Text)
			if p.Call != nil {
				bytes += len(p.Call.Name) + len(p.Call.Args)
			}
			if p.Thought != nil {
				bytes += len(p.Thought.Text)
			}
			if p.Media != nil {
				media += EstimateMediaTokens(*p.Media)
			}
			if p.Result != nil {
				bytes += len(p.Result.Output) + len(p.Result.Name)
				for _, md := range p.Result.Media {
					media += EstimateMediaTokens(md)
				}
			}
		}
	}
	return bytes/4 + media
}

// EstimateOutputTokens approximates the tokens of a response message.
func EstimateOutputTokens(m llm.Message) int {
	n := 0
	for _, p := range m.Parts {
		n += len(p.Text)
		if p.Call != nil {
			n += len(p.Call.Name) + len(p.Call.Args)
		}
		if p.Thought != nil {
			n += len(p.Thought.Text)
		}
	}
	return n / 4
}

// EstimateUsage fills a Usage from estimates, flagged as Estimated.
func EstimateUsage(system string, msgs []llm.Message, tools []llm.ToolSpec, out llm.Message) *llm.Usage {
	p, c := EstimateTokens(system, msgs, tools), EstimateOutputTokens(out)
	return &llm.Usage{PromptTokens: p, CompletionTokens: c, TotalTokens: p + c, Estimated: true}
}
