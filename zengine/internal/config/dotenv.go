// Package config loads and validates the agent's two configuration files
// (.env for provider credentials and model capabilities, config.json for
// everything else) and defines the on-disk layout of the agent home.
package config

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// ParseDotenv reads KEY=VALUE pairs. It supports comments, blank lines, an
// optional "export " prefix, and single or double quoted values (double
// quotes understand \n, \t, \\ and \"). Inline " #" comments are stripped from
// unquoted values.
func ParseDotenv(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := map[string]string{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := strings.TrimSpace(strings.TrimPrefix(sc.Text(), "\xef\xbb\xbf"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("%s:%d: expected KEY=VALUE, got %q", path, lineNo, line)
		}
		key = strings.TrimSpace(key)
		if key == "" {
			return nil, fmt.Errorf("%s:%d: empty key", path, lineNo)
		}
		v, err := parseValue(strings.TrimSpace(val))
		if err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, lineNo, err)
		}
		out[key] = v
	}
	return out, sc.Err()
}

func parseValue(v string) (string, error) {
	if v == "" {
		return "", nil
	}
	switch v[0] {
	case '"':
		end := -1
		for i := 1; i < len(v); i++ {
			if v[i] == '\\' {
				i++
				continue
			}
			if v[i] == '"' {
				end = i
				break
			}
		}
		if end < 0 {
			return "", fmt.Errorf("unterminated double-quoted value")
		}
		r := strings.NewReplacer(`\n`, "\n", `\t`, "\t", `\"`, `"`, `\\`, `\`)
		return r.Replace(v[1:end]), nil
	case '\'':
		end := strings.IndexByte(v[1:], '\'')
		if end < 0 {
			return "", fmt.Errorf("unterminated single-quoted value")
		}
		return v[1 : 1+end], nil
	}
	if i := strings.Index(v, " #"); i >= 0 {
		v = v[:i]
	}
	return strings.TrimSpace(v), nil
}
