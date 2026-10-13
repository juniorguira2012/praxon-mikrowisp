package proxy

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/axentra/mikrowisp-proxy/internal/config"
)

// strPtr / boolPtr keep the test cases readable. Mirrors the codegen
// pattern most Go projects use for *string / *bool field setup.
func strPtr(s string) *string { return &s }
func boolPtr(b bool) *bool    { return &b }

// extras is a tiny helper that turns a literal map into the
// json.RawMessage shape the evaluator consumes. Pulled out so each test
// case stays a one-liner.
func extras(t *testing.T, kv map[string]any) map[string]json.RawMessage {
	t.Helper()
	out := make(map[string]json.RawMessage, len(kv))
	for k, v := range kv {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("encode %q: %v", k, err)
		}
		out[k] = raw
	}
	return out
}

func TestEvalRules_ValueMatch(t *testing.T) {
	rules := []config.ExcludeRule{
		{Field: "tipo_cliente", Value: strPtr("informal")},
	}
	cases := []struct {
		name   string
		extras map[string]any
		want   bool
	}{
		{"matches", map[string]any{"tipo_cliente": "informal"}, true},
		{"different value", map[string]any{"tipo_cliente": "formal"}, false},
		{"missing field", map[string]any{"otra_cosa": "informal"}, false},
		{"empty extras", map[string]any{}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := evalRules(rules, extras(t, c.extras))
			if got != c.want {
				t.Errorf("evalRules: got %v want %v", got, c.want)
			}
		})
	}
}

func TestEvalRules_BooleanCoercion(t *testing.T) {
	// Mikrowisp returns booleans as JSON booleans, not strings. The
	// operator writes the rule as `value = "false"`. We coerce to the
	// canonical string form so the rule fires.
	rules := []config.ExcludeRule{
		{Field: "mantenimiento", Value: strPtr("true")},
	}
	if !evalRules(rules, extras(t, map[string]any{"mantenimiento": true})) {
		t.Error("boolean true should match value=\"true\"")
	}
	if evalRules(rules, extras(t, map[string]any{"mantenimiento": false})) {
		t.Error("boolean false should NOT match value=\"true\"")
	}
}

func TestEvalRules_NumberCoercion(t *testing.T) {
	rules := []config.ExcludeRule{
		{Field: "plan", Value: strPtr("100")},
	}
	if !evalRules(rules, extras(t, map[string]any{"plan": 100})) {
		t.Error("integer 100 should match value=\"100\"")
	}
	if evalRules(rules, extras(t, map[string]any{"plan": 200})) {
		t.Error("integer 200 should NOT match value=\"100\"")
	}
}

func TestEvalRules_EmptyStringMatch(t *testing.T) {
	// "field exists but is blank" is a real Mikrowisp shape (admin
	// defined the field but never filled it in for this customer). Rule
	// `value = ""` matches that exact case.
	rules := []config.ExcludeRule{
		{Field: "rnc_factura", Value: strPtr("")},
	}
	if !evalRules(rules, extras(t, map[string]any{"rnc_factura": ""})) {
		t.Error("blank field value should match value=\"\"")
	}
	if evalRules(rules, extras(t, map[string]any{})) {
		t.Error("absent field should NOT match value=\"\" (use present=false for that)")
	}
}

func TestEvalRules_PresentFalse(t *testing.T) {
	// `present = false` matches when the key is missing entirely.
	rules := []config.ExcludeRule{
		{Field: "rnc_factura", Present: boolPtr(false)},
	}
	if !evalRules(rules, extras(t, map[string]any{"otra": "x"})) {
		t.Error("missing field should match present=false")
	}
	if evalRules(rules, extras(t, map[string]any{"rnc_factura": "anything"})) {
		t.Error("present field should NOT match present=false")
	}
}

func TestEvalRules_PresentTrue(t *testing.T) {
	// `present = true` matches when the key exists, regardless of its
	// value. Useful for "drop any customer who has been flagged with a
	// specific extra at all".
	rules := []config.ExcludeRule{
		{Field: "blacklist", Present: boolPtr(true)},
	}
	if !evalRules(rules, extras(t, map[string]any{"blacklist": "yes"})) {
		t.Error("present field should match present=true")
	}
	if !evalRules(rules, extras(t, map[string]any{"blacklist": ""})) {
		t.Error("present-but-blank field should still match present=true")
	}
	if evalRules(rules, extras(t, map[string]any{})) {
		t.Error("absent field should NOT match present=true")
	}
}

func TestEvalRules_FirstMatchWins(t *testing.T) {
	// Multiple rules act as an OR. The first match short-circuits.
	rules := []config.ExcludeRule{
		{Field: "tipo_cliente", Value: strPtr("informal")},
		{Field: "etiqueta", Value: strPtr("test")},
	}
	if !evalRules(rules, extras(t, map[string]any{"etiqueta": "test"})) {
		t.Error("any matching rule should trigger exclusion")
	}
	if evalRules(rules, extras(t, map[string]any{"tipo_cliente": "regular", "etiqueta": "vip"})) {
		t.Error("no rule matches -> not excluded")
	}
}

func TestVerdictCache_GetPut(t *testing.T) {
	c := newVerdictCache(time.Minute, 100)
	if _, ok := c.Get(7); ok {
		t.Error("empty cache should miss")
	}
	c.Put(7, true)
	got, ok := c.Get(7)
	if !ok || !got {
		t.Errorf("Put then Get: got (%v, %v), want (true, true)", got, ok)
	}
}

func TestVerdictCache_TTL(t *testing.T) {
	c := newVerdictCache(10*time.Millisecond, 100)
	c.Put(1, true)
	time.Sleep(20 * time.Millisecond)
	if _, ok := c.Get(1); ok {
		t.Error("entry should expire after TTL")
	}
}

func TestVerdictCache_EvictsOnFull(t *testing.T) {
	// Cap at 5 entries; insert 10. The oldest 5 (or some 10% slice)
	// should have been dropped to make room. We don't care which
	// specific ids survive — just that the map respected the cap.
	c := newVerdictCache(time.Hour, 5)
	for i := 0; i < 10; i++ {
		c.Put(i, true)
	}
	c.mu.RLock()
	size := len(c.entries)
	c.mu.RUnlock()
	if size > 5 {
		t.Errorf("cache size %d exceeds max %d after eviction", size, 5)
	}
}
