package cli

import (
	"io"
	"reflect"
	"testing"
)

func TestNormalizeArgs(t *testing.T) {
	tests := []struct{ in, want []string }{
		{[]string{"--session", "work1", "--prompt", "hi"}, []string{"--session", "work1", "--prompt", "hi"}},
		{[]string{"--session", "--ui", "cli"}, []string{"--session=", "--ui", "cli"}},
		{[]string{"--ui", "cli", "--session"}, []string{"--ui", "cli", "--session="}},
		{[]string{"--output"}, []string{"--output=all"}},
		{[]string{"--output", "200"}, []string{"--output=200"}},
		{[]string{"--output", "all", "-q"}, []string{"--output=all", "-q"}},
		{[]string{"--output", "-q"}, []string{"--output=all", "-q"}},
		{[]string{"--prompt", "output 5"}, []string{"--prompt", "output 5"}},
		{[]string{"-s", "-v"}, []string{"--session=", "-v"}},
	}
	for _, tc := range tests {
		if got := normalizeArgs(tc.in); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%v\n got %v\nwant %v", tc.in, got, tc.want)
		}
	}
}

func TestParseOptions(t *testing.T) {
	o, err := parseOptions("zagent", []string{"--session", "--ui", "cli"}, io.Discard)
	if err != nil || !o.sessionSet || o.session != "" || o.ui != "cli" {
		t.Fatalf("bare --session: %+v %v", o, err)
	}
	o, err = parseOptions("zagent", []string{"-s", "job", "-p", "do it", "-a", "a.png", "--attach", "b.pdf", "--output", "50", "-w", "/proj", "-q"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if o.session != "job" || o.prompt != "do it" || len(o.attach) != 2 || !o.outputSet || o.outputN != 50 || o.workspace != "/proj" || !o.quiet {
		t.Fatalf("options: %+v", o)
	}
	o, _ = parseOptions("zagent", []string{"--output"}, io.Discard)
	if !o.outputSet || o.outputN != 0 {
		t.Fatalf("complete output: %+v", o)
	}
	o, _ = parseOptions("zagent", []string{"-p", "x"}, io.Discard)
	if o.outputSet || o.sessionSet || o.ui != "headless" {
		t.Fatalf("defaults: %+v", o)
	}
	for _, bad := range [][]string{{"--ui", "tui"}, {"--output=0"}, {"--output=abc"}, {"--nonsense"}} {
		if _, err := parseOptions("zagent", bad, io.Discard); err == nil {
			t.Errorf("%v should be rejected", bad)
		}
	}
}
