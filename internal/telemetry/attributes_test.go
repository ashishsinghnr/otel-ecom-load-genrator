package telemetry

import (
	"encoding/json"
	"testing"

	"go.opentelemetry.io/otel/attribute"
)

// decodeJSON mimics how attributes actually arrive: through encoding/json,
// which turns every number into float64 and every array into []interface{}.
func decodeJSON(t *testing.T, raw string) map[string]interface{} {
	t.Helper()
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("bad test JSON: %v", err)
	}
	return m
}

func find(kvs []attribute.KeyValue, key string) (attribute.KeyValue, bool) {
	for _, kv := range kvs {
		if string(kv.Key) == key {
			return kv, true
		}
	}
	return attribute.KeyValue{}, false
}

// C6: an integral JSON number must become an int attribute, not a float.
func TestConvertAttributes_IntegralNumbersBecomeInts(t *testing.T) {
	m := decodeJSON(t, `{"http.response.status_code": 503, "retries": 0, "big": 9007199254740991}`)
	kvs := ConvertAttributes(m)

	for _, key := range []string{"http.response.status_code", "retries", "big"} {
		kv, ok := find(kvs, key)
		if !ok {
			t.Fatalf("attribute %q missing", key)
		}
		if kv.Value.Type() != attribute.INT64 {
			t.Errorf("%q has type %v, want INT64", key, kv.Value.Type())
		}
	}

	kv, _ := find(kvs, "http.response.status_code")
	if kv.Value.AsInt64() != 503 {
		t.Errorf("status code = %d, want 503", kv.Value.AsInt64())
	}
}

func TestConvertAttributes_FractionalNumbersStayFloats(t *testing.T) {
	m := decodeJSON(t, `{"cart.value": 49.99, "rate": 0.5}`)
	kvs := ConvertAttributes(m)

	kv, ok := find(kvs, "cart.value")
	if !ok {
		t.Fatal("cart.value missing")
	}
	if kv.Value.Type() != attribute.FLOAT64 {
		t.Errorf("type = %v, want FLOAT64", kv.Value.Type())
	}
	if kv.Value.AsFloat64() != 49.99 {
		t.Errorf("value = %v, want 49.99", kv.Value.AsFloat64())
	}
}

// C5: JSON arrays decode to []interface{} and must not be dropped.
func TestConvertAttributes_JSONArraysAreNotDropped(t *testing.T) {
	m := decodeJSON(t, `{
		"tags": ["a", "b"],
		"codes": [200, 404],
		"ratios": [0.5, 1.5],
		"flags": [true, false]
	}`)
	kvs := ConvertAttributes(m)

	if len(kvs) != 4 {
		t.Fatalf("got %d attributes, want 4: %v", len(kvs), kvs)
	}

	tests := []struct {
		key      string
		wantType attribute.Type
	}{
		{"tags", attribute.STRINGSLICE},
		{"codes", attribute.INT64SLICE},
		{"ratios", attribute.FLOAT64SLICE},
		{"flags", attribute.BOOLSLICE},
	}
	for _, tc := range tests {
		kv, ok := find(kvs, tc.key)
		if !ok {
			t.Errorf("attribute %q missing", tc.key)
			continue
		}
		if kv.Value.Type() != tc.wantType {
			t.Errorf("%q has type %v, want %v", tc.key, kv.Value.Type(), tc.wantType)
		}
	}

	codes, _ := find(kvs, "codes")
	got := codes.Value.AsInt64Slice()
	if len(got) != 2 || got[0] != 200 || got[1] != 404 {
		t.Errorf("codes = %v, want [200 404]", got)
	}
}

func TestConvertAttributes_MixedArrayFallsBackToStrings(t *testing.T) {
	m := decodeJSON(t, `{"mixed": ["a", 1, true]}`)
	kvs := ConvertAttributes(m)

	kv, ok := find(kvs, "mixed")
	if !ok {
		t.Fatal("mixed missing")
	}
	if kv.Value.Type() != attribute.STRINGSLICE {
		t.Fatalf("type = %v, want STRINGSLICE", kv.Value.Type())
	}
	got := kv.Value.AsStringSlice()
	want := []string{"a", "1", "true"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("element %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestConvertAttributes_StringsAndBools(t *testing.T) {
	m := decodeJSON(t, `{"version": "v2.1", "cached": true}`)
	kvs := ConvertAttributes(m)

	v, ok := find(kvs, "version")
	if !ok || v.Value.AsString() != "v2.1" {
		t.Errorf("version = %v", v.Value)
	}
	c, ok := find(kvs, "cached")
	if !ok || !c.Value.AsBool() {
		t.Errorf("cached = %v", c.Value)
	}
}

// C10: the error directive controls span status and must not be exported as
// a plain attribute.
func TestConvertAttributes_SkipsErrorDirective(t *testing.T) {
	m := decodeJSON(t, `{"error": true, "http.response.status_code": 503}`)
	kvs := ConvertAttributes(m)

	if _, ok := find(kvs, "error"); ok {
		t.Error("the error directive must not be exported as an attribute")
	}
	if _, ok := find(kvs, "http.response.status_code"); !ok {
		t.Error("sibling attributes must still be exported")
	}
}

func TestConvertAttributes_EdgeCases(t *testing.T) {
	if got := ConvertAttributes(nil); got != nil {
		t.Errorf("nil map -> %v, want nil", got)
	}
	if got := ConvertAttributes(map[string]interface{}{}); got != nil {
		t.Errorf("empty map -> %v, want nil", got)
	}

	// A null value and an empty array carry nothing exportable.
	m := decodeJSON(t, `{"nothing": null, "empty": []}`)
	if got := ConvertAttributes(m); len(got) != 0 {
		t.Errorf("null and empty array produced %v, want none", got)
	}
}

func TestConvertAttributes_GoNativeTypes(t *testing.T) {
	m := map[string]interface{}{
		"i":   42,
		"i64": int64(7),
		"f32": float32(1.5),
		"ss":  []string{"x"},
		"is":  []int64{1, 2},
	}
	kvs := ConvertAttributes(m)
	if len(kvs) != 5 {
		t.Fatalf("got %d attributes, want 5: %v", len(kvs), kvs)
	}
	if kv, _ := find(kvs, "i"); kv.Value.AsInt64() != 42 {
		t.Errorf("i = %v, want 42", kv.Value)
	}
}

func TestIsIntegral(t *testing.T) {
	for _, f := range []float64{0, 1, -1, 503, 1e15} {
		if !isIntegral(f) {
			t.Errorf("isIntegral(%v) = false, want true", f)
		}
	}
	for _, f := range []float64{0.5, -1.25, 1e300} {
		if isIntegral(f) {
			t.Errorf("isIntegral(%v) = true, want false", f)
		}
	}
}
