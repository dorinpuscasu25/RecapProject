package ai

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

// ValidateSchema checks a decoded JSON value against a JSON Schema subset: type, properties,
// required, additionalProperties=false, enum, items, minItems/maxItems, minimum/maximum,
// maxLength. It is intentionally small and dependency-free; semantic checks live in Validators.
func ValidateSchema(schema map[string]any, v any) error {
	var errs []string
	validate(schema, v, "$", &errs)
	if len(errs) > 0 {
		sort.Strings(errs)
		if len(errs) > 10 {
			errs = errs[:10]
		}
		return fmt.Errorf("schema validation failed: %s", strings.Join(errs, "; "))
	}
	return nil
}

func validate(s map[string]any, v any, path string, errs *[]string) {
	if s == nil {
		return
	}
	add := func(f string, a ...any) { *errs = append(*errs, path+": "+fmt.Sprintf(f, a...)) }
	if alts, ok := s["anyOf"].([]any); ok {
		for _, alt := range alts {
			var sub []string
			if m, ok := alt.(map[string]any); ok {
				validate(m, v, path, &sub)
				if len(sub) == 0 {
					return
				}
			}
		}
		add("matches no anyOf alternative")
		return
	}
	if enum, ok := s["enum"].([]any); ok {
		found := false
		for _, e := range enum {
			if fmt.Sprint(e) == fmt.Sprint(v) {
				found = true
				break
			}
		}
		if !found {
			add("value %v not in enum", v)
		}
	}
	types := []string{}
	switch t := s["type"].(type) {
	case string:
		types = []string{t}
	case []any:
		for _, x := range t {
			types = append(types, fmt.Sprint(x))
		}
	}
	if len(types) > 0 {
		ok := false
		for _, t := range types {
			if typeMatches(t, v) {
				ok = true
				break
			}
		}
		if !ok {
			add("expected type %v, got %T", types, v)
			return
		}
	}
	switch x := v.(type) {
	case map[string]any:
		props, _ := s["properties"].(map[string]any)
		if req, ok := s["required"].([]any); ok {
			for _, r := range req {
				if _, ok := x[fmt.Sprint(r)]; !ok {
					add("missing required property %q", r)
				}
			}
		}
		for k, val := range x {
			ps, ok := props[k].(map[string]any)
			if !ok {
				if ap, ok := s["additionalProperties"].(bool); ok && !ap {
					add("unexpected property %q", k)
				}
				continue
			}
			validate(ps, val, path+"."+k, errs)
		}
	case []any:
		if n, ok := num(s["minItems"]); ok && float64(len(x)) < n {
			add("too few items")
		}
		if n, ok := num(s["maxItems"]); ok && float64(len(x)) > n {
			add("too many items")
		}
		if items, ok := s["items"].(map[string]any); ok {
			for i, it := range x {
				validate(items, it, fmt.Sprintf("%s[%d]", path, i), errs)
			}
		}
	case float64:
		if n, ok := num(s["minimum"]); ok && x < n {
			add("below minimum")
		}
		if n, ok := num(s["maximum"]); ok && x > n {
			add("above maximum")
		}
	case string:
		if n, ok := num(s["maxLength"]); ok && float64(len([]rune(x))) > n {
			add("string too long")
		}
	}
}

func typeMatches(t string, v any) bool {
	switch t {
	case "object":
		_, ok := v.(map[string]any)
		return ok
	case "array":
		_, ok := v.([]any)
		return ok
	case "string":
		_, ok := v.(string)
		return ok
	case "number":
		_, ok := v.(float64)
		return ok
	case "integer":
		f, ok := v.(float64)
		return ok && f == math.Trunc(f)
	case "boolean":
		_, ok := v.(bool)
		return ok
	case "null":
		return v == nil
	}
	return false
}

func num(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case int:
		return float64(x), true
	}
	return 0, false
}

// ProviderSchema strips keywords that some providers' structured-output modes reject
// (numeric/string constraints); the gateway still validates the full schema locally.
func ProviderSchema(s map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range s {
		switch k {
		case "minimum", "maximum", "maxLength", "minLength", "minItems", "maxItems", "multipleOf", "pattern":
			continue
		}
		switch x := v.(type) {
		case map[string]any:
			out[k] = ProviderSchema(x)
		case []any:
			arr := make([]any, len(x))
			for i, it := range x {
				if m, ok := it.(map[string]any); ok {
					arr[i] = ProviderSchema(m)
				} else {
					arr[i] = it
				}
			}
			out[k] = arr
		default:
			out[k] = v
		}
	}
	return out
}
