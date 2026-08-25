package telemetry

import (
	"math"
	"strconv"

	"go.opentelemetry.io/otel/attribute"

	"github.com/ashishsinghnr/otel-ecom-load-genrator/internal/config"
)

// ConvertAttributes turns decoded JSON values into OTel attributes.
//
// Two decoding facts drive the shape of this function:
//
//   - encoding/json decodes every JSON number to float64, so an integral
//     value must be detected and converted, or "status_code": 503 exports as
//     a float (spec C6).
//   - encoding/json decodes every JSON array to []interface{}, so typed-slice
//     cases such as []int never match and would silently drop the attribute
//     (spec C5).
//
// The "error" directive key is skipped: it controls span status rather than
// being exported as an attribute (spec C10).
func ConvertAttributes(m map[string]interface{}) []attribute.KeyValue {
	if len(m) == 0 {
		return nil
	}
	out := make([]attribute.KeyValue, 0, len(m))
	for k, v := range m {
		if k == config.ErrorDirectiveKey {
			continue
		}
		if kv, ok := convertOne(k, v); ok {
			out = append(out, kv)
		}
	}
	return out
}

func convertOne(k string, v interface{}) (attribute.KeyValue, bool) {
	switch val := v.(type) {
	case nil:
		return attribute.KeyValue{}, false

	case bool:
		return attribute.Bool(k, val), true

	case string:
		return attribute.String(k, val), true

	case float64:
		// JSON has one number type; preserve integers as integers.
		if isIntegral(val) {
			return attribute.Int64(k, int64(val)), true
		}
		return attribute.Float64(k, val), true

	// Present for callers that build maps in Go rather than from JSON.
	case int:
		return attribute.Int(k, val), true
	case int64:
		return attribute.Int64(k, val), true
	case float32:
		return attribute.Float64(k, float64(val)), true

	case []interface{}:
		return convertSlice(k, val)

	// Already-typed slices, for Go callers.
	case []string:
		return attribute.StringSlice(k, val), true
	case []bool:
		return attribute.BoolSlice(k, val), true
	case []int64:
		return attribute.Int64Slice(k, val), true
	case []float64:
		return attribute.Float64Slice(k, val), true

	default:
		return attribute.KeyValue{}, false
	}
}

// convertSlice handles the []interface{} that JSON arrays decode to.
//
// OTel attributes must be homogeneous slices, so the element type is taken
// from the first element and mixed arrays fall back to strings.
func convertSlice(k string, vals []interface{}) (attribute.KeyValue, bool) {
	if len(vals) == 0 {
		return attribute.KeyValue{}, false
	}

	switch vals[0].(type) {
	case bool:
		out := make([]bool, 0, len(vals))
		for _, v := range vals {
			b, ok := v.(bool)
			if !ok {
				return stringifySlice(k, vals)
			}
			out = append(out, b)
		}
		return attribute.BoolSlice(k, out), true

	case string:
		out := make([]string, 0, len(vals))
		for _, v := range vals {
			s, ok := v.(string)
			if !ok {
				return stringifySlice(k, vals)
			}
			out = append(out, s)
		}
		return attribute.StringSlice(k, out), true

	case float64:
		return convertNumberSlice(k, vals)

	default:
		return stringifySlice(k, vals)
	}
}

// convertNumberSlice renders a JSON number array as an int64 slice when every
// element is integral, and a float64 slice otherwise.
func convertNumberSlice(k string, vals []interface{}) (attribute.KeyValue, bool) {
	nums := make([]float64, 0, len(vals))
	allInt := true
	for _, v := range vals {
		f, ok := v.(float64)
		if !ok {
			return stringifySlice(k, vals)
		}
		if !isIntegral(f) {
			allInt = false
		}
		nums = append(nums, f)
	}

	if allInt {
		out := make([]int64, 0, len(nums))
		for _, f := range nums {
			out = append(out, int64(f))
		}
		return attribute.Int64Slice(k, out), true
	}
	return attribute.Float64Slice(k, nums), true
}

// stringifySlice is the fallback for heterogeneous or nested arrays, which
// OTel cannot represent directly.
func stringifySlice(k string, vals []interface{}) (attribute.KeyValue, bool) {
	out := make([]string, 0, len(vals))
	for _, v := range vals {
		out = append(out, stringify(v))
	}
	return attribute.StringSlice(k, out), true
}

func stringify(v interface{}) string {
	switch val := v.(type) {
	case string:
		return val
	case bool:
		if val {
			return "true"
		}
		return "false"
	case float64:
		if isIntegral(val) {
			return strconv.FormatInt(int64(val), 10)
		}
		return strconv.FormatFloat(val, 'g', -1, 64)
	case nil:
		return ""
	default:
		return "unsupported"
	}
}

// isIntegral reports whether f holds a whole number that fits in an int64.
func isIntegral(f float64) bool {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return false
	}
	if f != math.Trunc(f) {
		return false
	}
	return f >= math.MinInt64 && f <= math.MaxInt64
}
