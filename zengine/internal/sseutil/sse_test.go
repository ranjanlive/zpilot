package sseutil

import (
	"io"
	"strings"
	"testing"
)

func collect(t *testing.T, in string) []Event {
	t.Helper()
	r := NewReader(strings.NewReader(in))
	var out []Event
	for {
		ev, err := r.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		out = append(out, ev)
	}
}

func TestBasicEvents(t *testing.T) {
	evs := collect(t, "data: {\"a\":1}\n\ndata: {\"a\":2}\n\n")
	if len(evs) != 2 || evs[0].Data != `{"a":1}` || evs[1].Data != `{"a":2}` {
		t.Fatalf("got %+v", evs)
	}
}

func TestNamedEventsAndMultilineData(t *testing.T) {
	evs := collect(t, "event: message_start\ndata: line1\ndata: line2\n\n")
	if len(evs) != 1 || evs[0].Name != "message_start" || evs[0].Data != "line1\nline2" {
		t.Fatalf("got %+v", evs)
	}
}

func TestCommentsCRLFAndMissingTrailingBlank(t *testing.T) {
	evs := collect(t, ": keep-alive\r\n\r\ndata: x\r\n\r\ndata: tail")
	if len(evs) != 1 && len(evs) != 2 {
		t.Fatalf("got %+v", evs)
	}
	last := evs[len(evs)-1]
	if last.Data != "tail" {
		t.Fatalf("unterminated final event lost: %+v", evs)
	}
}

func TestVeryLongLine(t *testing.T) {
	big := strings.Repeat("x", 5<<20) // 5 MiB, far past bufio.Scanner's default
	evs := collect(t, "data: "+big+"\n\n")
	if len(evs) != 1 || len(evs[0].Data) != len(big) {
		t.Fatalf("long line truncated: %d events, len=%d", len(evs), len(evs[0].Data))
	}
}

func TestDataWithoutSpaceAfterColon(t *testing.T) {
	evs := collect(t, "data:{\"k\":true}\n\n")
	if len(evs) != 1 || evs[0].Data != `{"k":true}` {
		t.Fatalf("got %+v", evs)
	}
}
