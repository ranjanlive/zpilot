// Package sseutil implements a small, strict reader for Server-Sent Events
// streams as emitted by LLM APIs. It exists instead of bufio.Scanner because
// stream lines can be arbitrarily long (inline base64) and Scanner's token
// limit would truncate them.
package sseutil

import (
	"bufio"
	"errors"
	"io"
	"strings"
)

// Event is one dispatched SSE event.
type Event struct {
	Name string // value of the "event:" field, if any
	Data string // data lines joined with "\n"
}

// Reader reads events from a stream.
type Reader struct {
	br *bufio.Reader
}

// NewReader wraps r.
func NewReader(r io.Reader) *Reader {
	return &Reader{br: bufio.NewReaderSize(r, 64*1024)}
}

// Next returns the next event. It returns io.EOF when the stream ends cleanly
// (including a final event not terminated by a blank line). Comment lines and
// unknown fields are skipped.
func (r *Reader) Next() (Event, error) {
	var (
		ev       Event
		dataBuf  []string
		haveData bool
	)
	flush := func() (Event, bool) {
		if !haveData && ev.Name == "" {
			return Event{}, false
		}
		ev.Data = strings.Join(dataBuf, "\n")
		return ev, true
	}
	for {
		line, err := r.br.ReadString('\n')
		if len(line) > 0 {
			line = strings.TrimRight(line, "\r\n")
			switch {
			case line == "":
				if out, ok := flush(); ok {
					return out, nil
				}
			case strings.HasPrefix(line, ":"):
				// comment / keep-alive
			default:
				field, value, _ := strings.Cut(line, ":")
				value = strings.TrimPrefix(value, " ")
				switch field {
				case "event":
					ev.Name = value
				case "data":
					dataBuf = append(dataBuf, value)
					haveData = true
				}
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				if out, ok := flush(); ok {
					return out, nil
				}
				return Event{}, io.EOF
			}
			return Event{}, err
		}
	}
}
