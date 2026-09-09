// Package util holds small, dependency-free helpers shared across the app
// package and the domain packages extracted from it.
package util

// NumberAsFloat converts a JSON-decoded number (float64, int, int64) to
// float64, so integer-valued JSON numbers are not dropped by a raw float64
// type assertion. It reports whether the value was numeric.
func NumberAsFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	default:
		return 0, false
	}
}

// ToString converts a value to string, returning "" for non-strings.
func ToString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}
