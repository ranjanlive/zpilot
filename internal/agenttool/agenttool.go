// Package agenttool implements "zpilot agent-tool <name>": the executables
// behind the collaboration tools that ZPilot gives agents (list_agents,
// ask_agent, tell_agent, manage_status, manage_propose).
//
// The engine runs them with the call's arguments as JSON on stdin and expects
// one JSON object {"output": "...", "is_error": bool} on stdout. They talk to
// the daemon's loopback RPC using the agent's own token (from the manifest's
// env) and the engine session name, which identifies the conversation.
package agenttool

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

type result struct {
	Output  string `json:"output"`
	IsError bool   `json:"is_error"`
}

// Main runs one tool call and returns the process exit code.
func Main(name string, stdin io.Reader, stdout io.Writer) int {
	out := run(name, stdin)
	_ = json.NewEncoder(stdout).Encode(out)
	return 0
}

type client struct {
	base, token, session string
	http                 *http.Client
}

func run(name string, stdin io.Reader) result {
	c := &client{
		base:    strings.TrimRight(os.Getenv("ZPILOT_URL"), "/"),
		token:   os.Getenv("ZPILOT_AGENT_TOKEN"),
		session: os.Getenv("ZAGENT_SESSION"),
		http:    &http.Client{Timeout: 90 * time.Second},
	}
	if c.base == "" || c.token == "" {
		return fail("this tool must be run by an agent managed by ZPilot (ZPILOT_URL / ZPILOT_AGENT_TOKEN are missing)")
	}
	raw, _ := io.ReadAll(io.LimitReader(stdin, 1<<20))
	var args map[string]json.RawMessage
	if len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, &args); err != nil {
			return fail("arguments are not a JSON object: " + err.Error())
		}
	}
	str := func(k string) string {
		var s string
		_ = json.Unmarshal(args[k], &s)
		return s
	}
	switch name {
	case "list_agents":
		var dir []map[string]any
		if err := c.do("GET", "/rpc/agents", nil, &dir); err != nil {
			return fail(err.Error())
		}
		if len(dir) == 0 {
			return ok("You have no contacts. The owner can add contacts on your agent settings page.")
		}
		var b strings.Builder
		b.WriteString("Agents you can contact:\n")
		for _, d := range dir {
			busy := ""
			if d["busy"] == true {
				busy = " (busy right now; requests will queue)"
			}
			fmt.Fprintf(&b, "- %v (%v), level %v%s: %v\n", d["username"], d["name"], d["level"], busy, d["description"])
		}
		return ok(b.String())
	case "ask_agent", "tell_agent":
		to, msg := str("to"), str("message")
		path := "/rpc/ask"
		if name == "tell_agent" {
			path = "/rpc/tell"
		}
		var sent struct {
			ThreadID  string `json:"thread_id"`
			MessageID string `json:"message_id"`
			To        string `json:"to"`
		}
		if err := c.do("POST", path, map[string]string{"to": to, "message": msg}, &sent); err != nil {
			return fail(err.Error())
		}
		if name == "tell_agent" {
			return ok(fmt.Sprintf("Sent to %s. Their reply will arrive later as a new message in this conversation; continue with other work or finish your turn.", sent.To))
		}
		return c.wait(sent.ThreadID, sent.MessageID, sent.To)
	case "manage_status":
		var v any
		if err := c.do("POST", "/rpc/manage/status", map[string]string{"what": str("what")}, &v); err != nil {
			return fail(err.Error())
		}
		b, _ := json.MarshalIndent(v, "", "  ")
		return ok(string(b))
	case "manage_propose":
		body := map[string]json.RawMessage{}
		for _, k := range []string{"action", "target", "payload", "reason"} {
			if v, ok := args[k]; ok {
				body[k] = v
			}
		}
		var p struct {
			ID     string `json:"id"`
			Action string `json:"action"`
			Target string `json:"target"`
		}
		if err := c.do("POST", "/rpc/manage/propose", body, &p); err != nil {
			return fail(err.Error())
		}
		return ok(fmt.Sprintf("Proposal %s (%s %s) is waiting for the owner's approval. Nothing has changed yet. Tell the owner they can approve it on the Approvals page or with: zpilot approvals approve %s", p.ID, p.Action, p.Target, p.ID))
	}
	return fail("unknown tool " + name)
}

// wait polls until the request has an answer. The engine's own run timeout
// bounds how long this can take.
func (c *client) wait(threadID, msgID, to string) result {
	for {
		var w struct {
			Done   bool   `json:"done"`
			Status string `json:"status"`
			Kind   string `json:"kind"`
			Text   string `json:"text"`
		}
		q := url.Values{"thread": {threadID}, "id": {msgID}, "timeout": {"55"}}
		if err := c.do("GET", "/rpc/wait?"+q.Encode(), nil, &w); err != nil {
			return fail("lost contact with ZPilot while waiting for " + to + ": " + err.Error())
		}
		if !w.Done {
			continue
		}
		switch w.Kind {
		case "reply":
			return ok(fmt.Sprintf("%s answered:\n\n%s", to, w.Text))
		default:
			return fail(fmt.Sprintf("%s could not complete the request (%s): %s", to, w.Status, w.Text))
		}
	}
}

func (c *client) do(method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.base+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("X-ZPilot-Session", c.session)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("ZPilot is not reachable: %v", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(data, &e) == nil && e.Error != "" {
			return fmt.Errorf("%s", e.Error)
		}
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

func ok(s string) result   { return result{Output: s} }
func fail(s string) result { return result{Output: s, IsError: true} }
