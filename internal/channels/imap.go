package channels

import (
	"bufio"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"
)

// imapClient is the small subset of IMAP4rev1 needed to read new mail:
// LOGIN, SELECT, UID SEARCH, UID FETCH BODY.PEEK[], UID STORE, LOGOUT.
type imapClient struct {
	conn net.Conn
	r    *bufio.Reader
	tag  int
}

func dialIMAP(addr string, useTLS bool, timeout time.Duration) (*imapClient, error) {
	host, _, _ := net.SplitHostPort(addr)
	d := &net.Dialer{Timeout: timeout}
	var conn net.Conn
	var err error
	if useTLS {
		conn, err = tls.DialWithDialer(d, "tcp", addr, &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12})
	} else {
		conn, err = d.Dial("tcp", addr)
	}
	if err != nil {
		return nil, err
	}
	c := &imapClient{conn: conn, r: bufio.NewReader(conn)}
	_ = conn.SetDeadline(time.Now().Add(timeout))
	greet, err := c.r.ReadString('\n')
	if err != nil {
		conn.Close()
		return nil, err
	}
	if !strings.HasPrefix(greet, "* OK") {
		conn.Close()
		return nil, fmt.Errorf("imap: unexpected greeting %q", strings.TrimSpace(greet))
	}
	return c, nil
}

func (c *imapClient) close() { c.conn.Close() }

// quote makes an IMAP quoted string.
func quote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// cmd sends a command and returns the untagged response lines and literals.
// Each element of lines is one logical response with any literal inlined.
func (c *imapClient) cmd(format string, args ...any) ([]string, error) {
	c.tag++
	tag := fmt.Sprintf("z%d", c.tag)
	line := fmt.Sprintf(format, args...)
	if strings.ContainsAny(line, "\r\n") {
		return nil, errors.New("imap: invalid characters in command")
	}
	_ = c.conn.SetDeadline(time.Now().Add(2 * time.Minute))
	if _, err := io.WriteString(c.conn, tag+" "+line+"\r\n"); err != nil {
		return nil, err
	}
	var out []string
	for {
		resp, err := c.readResponse()
		if err != nil {
			return nil, err
		}
		if strings.HasPrefix(resp, tag+" ") {
			status := strings.TrimPrefix(resp, tag+" ")
			if !strings.HasPrefix(status, "OK") {
				return out, fmt.Errorf("imap: %s", strings.TrimSpace(status))
			}
			return out, nil
		}
		out = append(out, resp)
	}
}

// readResponse reads one line, following {n} literals.
func (c *imapClient) readResponse() (string, error) {
	var b strings.Builder
	for {
		line, err := c.r.ReadString('\n')
		if err != nil {
			return "", err
		}
		line = strings.TrimRight(line, "\r\n")
		if n, ok := literalSize(line); ok {
			if n > 50<<20 {
				return "", errors.New("imap: message too large")
			}
			b.WriteString(line)
			b.WriteString("\n")
			lit := make([]byte, n)
			if _, err := io.ReadFull(c.r, lit); err != nil {
				return "", err
			}
			b.Write(lit)
			continue
		}
		b.WriteString(line)
		return b.String(), nil
	}
}

func literalSize(line string) (int, bool) {
	if !strings.HasSuffix(line, "}") {
		return 0, false
	}
	i := strings.LastIndex(line, "{")
	if i < 0 {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimSuffix(line[i+1:len(line)-1], "+"))
	return n, err == nil
}

func (c *imapClient) login(user, pass string) error {
	_, err := c.cmd("LOGIN %s %s", quote(user), quote(pass))
	return err
}

// searchUnseen returns UIDs of unseen messages received since the date.
func (c *imapClient) searchUnseen(since time.Time) ([]int, error) {
	lines, err := c.cmd("UID SEARCH UNSEEN SINCE %s", since.Format("2-Jan-2006"))
	if err != nil {
		return nil, err
	}
	var uids []int
	for _, l := range lines {
		if rest, ok := strings.CutPrefix(l, "* SEARCH"); ok {
			for _, f := range strings.Fields(rest) {
				if n, err := strconv.Atoi(f); err == nil {
					uids = append(uids, n)
				}
			}
		}
	}
	return uids, nil
}

// fetch returns the raw RFC 822 message for a UID without marking it seen.
func (c *imapClient) fetch(uid int) ([]byte, error) {
	lines, err := c.cmd("UID FETCH %d (BODY.PEEK[])", uid)
	if err != nil {
		return nil, err
	}
	for _, l := range lines {
		head, body, ok := strings.Cut(l, "\n")
		if ok && strings.Contains(head, "FETCH") {
			// strip the closing ")" that follows the literal
			body = strings.TrimSuffix(strings.TrimRight(body, " \r\n"), ")")
			return []byte(body), nil
		}
	}
	return nil, fmt.Errorf("imap: message %d not returned", uid)
}

func (c *imapClient) markSeen(uid int) error {
	_, err := c.cmd(`UID STORE %d +FLAGS.SILENT (\Seen)`, uid)
	return err
}
