package offices

import (
	"fmt"
	"math"
	"regexp"
	"sort"
)

// ValidateSchema checks a value against the subset of JSON Schema a plugin's
// config_schema may use: type, properties, required, additionalProperties
// (a boolean or a schema), items, enum, pattern, minimum, maximum, minItems,
// description. A keyword outside that subset is an error, not ignored, so a
// schema never looks stricter than it is.
func ValidateSchema(schema any, v any) error {
	return validate(schema, v, "config")
}

var schemaKeys = map[string]bool{"type": true, "properties": true, "required": true, "additionalProperties": true,
	"items": true, "enum": true, "pattern": true, "minimum": true, "maximum": true, "minItems": true,
	"description": true, "$schema": true, "title": true}

func validate(schema any, v any, at string) error {
	s, ok := schema.(map[string]any)
	if !ok {
		if b, ok := schema.(bool); ok {
			if !b {
				return fmt.Errorf("%s: not allowed", at)
			}
			return nil
		}
		return fmt.Errorf("%s: the schema is not an object", at)
	}
	for k := range s {
		if !schemaKeys[k] {
			return fmt.Errorf("%s: schema keyword %q isn't supported", at, k)
		}
	}
	if t, ok := s["type"]; ok {
		var types []string
		switch x := t.(type) {
		case string:
			types = []string{x}
		case []any:
			for _, e := range x {
				if str, ok := e.(string); ok {
					types = append(types, str)
				}
			}
		}
		match := false
		for _, ty := range types {
			if typeIs(ty, v) {
				match = true
			}
		}
		if !match {
			return fmt.Errorf("%s: want %v", at, t)
		}
	}
	if e, ok := s["enum"].([]any); ok {
		found := false
		for _, x := range e {
			if fmt.Sprint(x) == fmt.Sprint(v) && typeName(x) == typeName(v) {
				found = true
			}
		}
		if !found {
			return fmt.Errorf("%s: %v is not one of %v", at, v, e)
		}
	}
	if p, ok := s["pattern"].(string); ok {
		if str, ok := v.(string); ok {
			re, err := regexp.Compile(p)
			if err != nil {
				return fmt.Errorf("%s: bad pattern: %v", at, err)
			}
			if !re.MatchString(str) {
				return fmt.Errorf("%s: %q doesn't match %s", at, str, p)
			}
		}
	}
	if n, ok := num(v); ok {
		if m, ok := num(s["minimum"]); ok && n < m {
			return fmt.Errorf("%s: %v is below %v", at, n, m)
		}
		if m, ok := num(s["maximum"]); ok && n > m {
			return fmt.Errorf("%s: %v is above %v", at, n, m)
		}
	}
	if arr, ok := v.([]any); ok {
		if m, ok := num(s["minItems"]); ok && float64(len(arr)) < m {
			return fmt.Errorf("%s: needs at least %v items", at, m)
		}
		if it, ok := s["items"]; ok {
			for i, e := range arr {
				if err := validate(it, e, fmt.Sprintf("%s[%d]", at, i)); err != nil {
					return err
				}
			}
		}
	}
	if obj, ok := v.(map[string]any); ok {
		props, _ := s["properties"].(map[string]any)
		if req, ok := s["required"].([]any); ok {
			for _, r := range req {
				if k, ok := r.(string); ok {
					if _, has := obj[k]; !has {
						return fmt.Errorf("%s: %s is required", at, k)
					}
				}
			}
		}
		keys := make([]string, 0, len(obj))
		for k := range obj {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if ps, ok := props[k]; ok {
				if err := validate(ps, obj[k], at+"."+k); err != nil {
					return err
				}
				continue
			}
			if ap, ok := s["additionalProperties"]; ok {
				if err := validate(ap, obj[k], at+"."+k); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func num(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case int64:
		return float64(x), true
	case int:
		return float64(x), true
	}
	return 0, false
}

func typeName(v any) string {
	switch v.(type) {
	case string:
		return "string"
	case bool:
		return "boolean"
	case nil:
		return "null"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	}
	if _, ok := num(v); ok {
		return "number"
	}
	return "?"
}

func typeIs(t string, v any) bool {
	switch t {
	case "integer":
		n, ok := num(v)
		return ok && n == math.Trunc(n)
	case "number":
		_, ok := num(v)
		return ok
	}
	return typeName(v) == t
}
