package control

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/ranjanlive/zpilot/zengine/internal/engine"
	"github.com/ranjanlive/zpilot/zengine/internal/session"
	"github.com/ranjanlive/zpilot/zengine/internal/sseutil"
)

// ErrNotRunning means no live engine owns the session.
var ErrNotRunning = errors.New("no engine is running for this session")

// Client talks to an engine's control server.
type Client struct {
	base  string
	token string
	hc    *http.Client
	// PID of the engine process, from the lock.
	PID int
}

// Attach connects to the engine that owns the session directory, using the
// endpoint published in its lock file.
func Attach(sessionDir string) (*Client, error) {
	li := session.ReadLock(sessionDir)
	if li == nil || li.Addr == "" {
		return nil, ErrNotRunning
	}
	return &Client{base: "http://" + li.Addr, token: li.Token, PID: li.PID,
		hc: &http.Client{Timeout: 0}}, nil
}

// New connects to an explicit address and token (tests, remote embedding).
func New(addr, token string) *Client {
	return &Client{base: "http://" + addr, token: token, hc: &http.Client{}}
}

func (c *Client) do(ctx context.Context, method, path string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrNotRunning, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		var e ErrorResponse
		_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&e)
		if e.Error == "" {
			e.Error = resp.Status
		}
		return errors.New(e.Error)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// State fetches the engine's state.
func (c *Client) State(ctx context.Context) (StateResponse, error) {
	var s StateResponse
	err := c.do(ctx, http.MethodGet, "/api/state", nil, &s)
	return s, err
}

// Events returns log events with Seq greater than since.
func (c *Client) Events(ctx context.Context, since int64) ([]session.Event, error) {
	var evs []session.Event
	err := c.do(ctx, http.MethodGet, "/api/events?since="+strconv.FormatInt(since, 10), nil, &evs)
	return evs, err
}

// Prompt queues a prompt.
func (c *Client) Prompt(ctx context.Context, text string, attachments ...string) error {
	return c.do(ctx, http.MethodPost, "/api/prompt", PromptRequest{Text: text, Attachments: attachments}, nil)
}

// Note sends a note for the agent's next request.
func (c *Client) Note(ctx context.Context, text string) error {
	return c.do(ctx, http.MethodPost, "/api/note", TextRequest{Text: text}, nil)
}

// Command runs a slash command on the engine.
func (c *Client) Command(ctx context.Context, line string) (engine.CommandResult, error) {
	var r engine.CommandResult
	err := c.do(ctx, http.MethodPost, "/api/command", CommandRequest{Line: line}, &r)
	return r, err
}

// Stop interrupts the agent's current work.
func (c *Client) Stop(ctx context.Context) error {
	return c.do(ctx, http.MethodPost, "/api/stop", nil, nil)
}

// Resume retries immediately when the engine is paused.
func (c *Client) Resume(ctx context.Context) error {
	return c.do(ctx, http.MethodPost, "/api/resume", nil, nil)
}

// Shutdown asks the engine process to end.
func (c *Client) Shutdown(ctx context.Context) error {
	return c.do(ctx, http.MethodPost, "/api/shutdown", nil, nil)
}

// Upload sends a local file to the engine's scratchpad and returns the path
// the agent can read it at.
func (c *Client) Upload(ctx context.Context, localPath string) (string, error) {
	f, err := os.Open(localPath)
	if err != nil {
		return "", err
	}
	defer f.Close()
	q := url.Values{"name": {baseName(localPath)}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/api/upload?"+q.Encode(), f)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var out UploadResponse
	if resp.StatusCode/100 != 2 {
		var e ErrorResponse
		_ = json.NewDecoder(resp.Body).Decode(&e)
		return "", errors.New(e.Error)
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	return out.Path, nil
}

func baseName(p string) string {
	p = strings.ReplaceAll(p, "\\", "/")
	if i := strings.LastIndex(p, "/"); i >= 0 {
		p = p[i+1:]
	}
	return p
}

// Stream delivers log events after `since` and live events until ctx ends or
// the connection drops. It returns nil on a clean ctx cancel and an error when
// the engine goes away, so callers can decide to reattach.
func (c *Client) Stream(ctx context.Context, since int64, onEvent func(session.Event), onLive func(engine.Live)) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/api/stream?since="+strconv.FormatInt(since, 10), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "text/event-stream")
	resp, err := c.hc.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return fmt.Errorf("%w: %v", ErrNotRunning, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("stream refused: %s", resp.Status)
	}
	sr := sseutil.NewReader(resp.Body)
	for {
		ev, err := sr.Next()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if errors.Is(err, io.EOF) {
				return fmt.Errorf("%w: the engine closed the stream", ErrNotRunning)
			}
			return err
		}
		switch ev.Name {
		case "event":
			var e session.Event
			if json.Unmarshal([]byte(ev.Data), &e) == nil && onEvent != nil {
				onEvent(e)
			}
		case "live":
			var l engine.Live
			if json.Unmarshal([]byte(ev.Data), &l) == nil && onLive != nil {
				onLive(l)
			}
		}
	}
}

// WaitReady polls until the engine for sessionDir answers, or the timeout passes.
func WaitReady(ctx context.Context, sessionDir string, timeout time.Duration) (*Client, error) {
	deadline := time.Now().Add(timeout)
	for {
		if c, err := Attach(sessionDir); err == nil {
			cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
			_, serr := c.State(cctx)
			cancel()
			if serr == nil {
				return c, nil
			}
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("the engine did not become ready within %s", timeout)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// WebURL returns the browser URL for this engine's web UI, with the token in
// the fragment so it is never sent to a server or logged.
func (c *Client) WebURL() string { return c.base + "/#token=" + c.token }
