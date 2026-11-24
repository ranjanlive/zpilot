package llm

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
)

// ValidateArgs checks raw tool-call arguments against a JSON Schema and
// returns human-readable problems (empty when valid). The messages are written
// for the model: they name the offending argument and say what is allowed, so
// the model can correct the call on its next attempt.
//
// Supported keywords: type, properties, required, additionalProperties, enum,
// items, minimum, maximum, minItems, maxItems. Unknown keywords are ignored.
func ValidateArgs(args json.RawMessage, schema json.RawMessage) []string {
	var sch map[string]any
	if len(bytes.TrimSpace(schema)) == 0 {
		return nil
	}
	if err := json.Unmarshal(schema, &sch); err != nil {
		return nil // a broken tool schema must not block the call
	}
	trimmed := bytes.TrimSpace(args)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		trimmed = []byte("{}")
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.UseNumber()
	var val any
	if err := dec.Decode(&val); err != nil {
		return []string{fmt.Sprintf("arguments are not valid JSON: %v", err)}
	}
	var problems []string
	validateNode(val, sch, "", &problems)
	return problems
}

func validateNode(val any, sch map[string]any, path string, out *[]string) {
	label := path
	if label == "" {
		label = "arguments"
	} else {
		label = fmt.Sprintf("argument %q", path)
	}

	if t, ok := sch["type"]; ok {
		if !typeMatches(val, t) {
			*out = append(*out, fmt.Sprintf("%s: expected %s, got %s", label, typeString(t), jsonTypeOf(val)))
			return
		}
	}
	if enum, ok := sch["enum"].([]any); ok {
		found := false
		for _, e := range enum {
			if fmt.Sprint(e) == fmt.Sprint(val) {
				found = true
				break
			}
		}
		if !found {
			opts := make([]string, len(enum))
			for i, e := range enum {
				opts[i] = fmt.Sprint(e)
			}
			*out = append(*out, fmt.Sprintf("%s: %v is not one of [%s]", label, val, strings.Join(opts, ", ")))
		}
	}

	switch v := val.(type) {
	case map[string]any:
		props, _ := sch["properties"].(map[string]any)
		if req, ok := sch["required"].([]any); ok {
			for _, r := range req {
				name, _ := r.(string)
				if _, present := v[name]; !present && name != "" {
					*out = append(*out, fmt.Sprintf("missing required argument %q", joinPath(path, name)))
				}
			}
		}
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			sub, known := props[k].(map[string]any)
			if !known {
				switch ap := sch["additionalProperties"].(type) {
				case bool:
					if !ap {
						*out = append(*out, fmt.Sprintf("unknown argument %q (allowed: %s)", joinPath(path, k), allowedList(props)))
					}
				case map[string]any:
					validateNode(v[k], ap, joinPath(path, k), out)
				}
				continue
			}
			validateNode(v[k], sub, joinPath(path, k), out)
		}
	case []any:
		if items, ok := sch["items"].(map[string]any); ok {
			for i, el := range v {
				validateNode(el, items, fmt.Sprintf("%s[%d]", path, i), out)
			}
		}
		if mn, ok := numberOf(sch["minItems"]); ok && float64(len(v)) < mn {
			*out = append(*out, fmt.Sprintf("%s: needs at least %v items", label, mn))
		}
		if mx, ok := numberOf(sch["maxItems"]); ok && float64(len(v)) > mx {
			*out = append(*out, fmt.Sprintf("%s: allows at most %v items", label, mx))
		}
	case json.Number:
		f, _ := v.Float64()
		if mn, ok := numberOf(sch["minimum"]); ok && f < mn {
			*out = append(*out, fmt.Sprintf("%s: %v is below the minimum %v", label, v, mn))
		}
		if mx, ok := numberOf(sch["maximum"]); ok && f > mx {
			*out = append(*out, fmt.Sprintf("%s: %v is above the maximum %v", label, v, mx))
		}
	}
}

func joinPath(parent, child string) string {
	if parent == "" {
		return child
	}
	return parent + "." + child
}

func allowedList(props map[string]any) string {
	if len(props) == 0 {
		return "none"
	}
	names := make([]string, 0, len(props))
	for k := range props {
		names = append(names, k)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

func numberOf(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}

func typeMatches(val any, t any) bool {
	switch tt := t.(type) {
	case string:
		return matchesOne(val, tt)
	case []any:
		for _, x := range tt {
			if s, ok := x.(string); ok && matchesOne(val, s) {
				return true
			}
		}
		return false
	}
	return true
}

func matchesOne(val any, t string) bool {
	switch t {
	case "string":
		_, ok := val.(string)
		return ok
	case "boolean":
		_, ok := val.(bool)
		return ok
	case "null":
		return val == nil
	case "object":
		_, ok := val.(map[string]any)
		return ok
	case "array":
		_, ok := val.([]any)
		return ok
	case "number":
		_, ok := val.(json.Number)
		return ok
	case "integer":
		n, ok := val.(json.Number)
		if !ok {
			return false
		}
		f, err := n.Float64()
		return err == nil && f == math.Trunc(f)
	}
	return true
}

func typeString(t any) string {
	switch tt := t.(type) {
	case string:
		return tt
	case []any:
		parts := make([]string, 0, len(tt))
		for _, x := range tt {
			parts = append(parts, fmt.Sprint(x))
		}
		return strings.Join(parts, " or ")
	}
	return fmt.Sprint(t)
}

func jsonTypeOf(val any) string {
	switch v := val.(type) {
	case nil:
		return "null"
	case string:
		return "string"
	case bool:
		return "boolean"
	case json.Number:
		if f, err := v.Float64(); err == nil && f == math.Trunc(f) {
			return "integer"
		}
		return "number"
	case map[string]any:
		return "object"
	case []any:
		return "array"
	}
	return fmt.Sprintf("%T", val)
}
