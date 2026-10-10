package executor

import (
	"testing"

	"github.com/tidwall/gjson"
)

// Shapes observed in production 400s on 2026-10-10 (group 21 inventory).
func TestAntigravityToolCompatProductionShapes(t *testing.T) {
	cases := []struct {
		name  string
		body  string
		check func(t *testing.T, out string)
	}{
		{
			name: "strict on native declaration",
			body: `{"request":{"tools":[{"functionDeclarations":[{"name":"f","strict":true,"parameters":{"type":"object","properties":{"a":{"type":"string"}}}}]}]}}`,
			check: func(t *testing.T, out string) {
				if gjson.Get(out, "request.tools.0.functionDeclarations.0.strict").Exists() {
					t.Fatalf("strict survived: %s", out)
				}
				if gjson.Get(out, "request.tools.0.functionDeclarations.0.name").String() != "f" {
					t.Fatalf("name lost: %s", out)
				}
			},
		},
		{
			name: "properties on string type",
			body: `{"request":{"tools":[{"functionDeclarations":[{"name":"f","parameters":{"type":"object","properties":{"intent":{"type":"string","properties":{"x":{"type":"string"}},"required":["x"]}}}}]}]}}`,
			check: func(t *testing.T, out string) {
				p := "request.tools.0.functionDeclarations.0.parameters.properties.intent"
				if gjson.Get(out, p+".properties").Exists() || gjson.Get(out, p+".required").Exists() {
					t.Fatalf("object-only keywords survived on string: %s", gjson.Get(out, p).Raw)
				}
			},
		},
		{
			name: "items properties on string items",
			body: `{"request":{"tools":[{"functionDeclarations":[{"name":"f","parameters":{"type":"object","properties":{"remove":{"type":"array","items":{"type":"string","properties":{"k":{"type":"string"}}}}}}}]}]}}`,
			check: func(t *testing.T, out string) {
				p := "request.tools.0.functionDeclarations.0.parameters.properties.remove.items"
				if gjson.Get(out, p+".properties").Exists() {
					t.Fatalf("properties survived on string items: %s", gjson.Get(out, p).Raw)
				}
			},
		},
		{
			name: "nested array items without items",
			body: `{"request":{"tools":[{"functionDeclarations":[{"name":"f","parameters":{"type":"object","properties":{"words":{"type":"array","items":{"type":"array"}}}}}]}]}}`,
			check: func(t *testing.T, out string) {
				p := "request.tools.0.functionDeclarations.0.parameters.properties.words.items.items"
				if !gjson.Get(out, p).IsObject() {
					t.Fatalf("inner array missing items: %s", gjson.Get(out, "request.tools.0.functionDeclarations.0.parameters").Raw)
				}
			},
		},
		{
			name: "tuple items list",
			body: `{"request":{"tools":[{"functionDeclarations":[{"name":"f","parameters":{"type":"object","properties":{"o":{"type":"object","properties":{"pair":{"type":"array","items":[{"type":"string"},{"type":"number"}]}}}}}}]}]}}`,
			check: func(t *testing.T, out string) {
				p := "request.tools.0.functionDeclarations.0.parameters.properties.o.properties.pair.items"
				if !gjson.Get(out, p).IsObject() {
					t.Fatalf("items still not an object: %s", gjson.Get(out, p).Raw)
				}
			},
		},
		{
			name: "def keyword",
			body: `{"request":{"tools":[{"functionDeclarations":[{"name":"f","parameters":{"type":"object","def":{"x":{"type":"string"}},"properties":{"a":{"type":"string"}}}}]}]}}`,
			check: func(t *testing.T, out string) {
				if gjson.Get(out, "request.tools.0.functionDeclarations.0.parameters.def").Exists() {
					t.Fatalf("def survived: %s", out)
				}
			},
		},
		{
			name: "business property named strict is kept",
			body: `{"request":{"tools":[{"functionDeclarations":[{"name":"f","parameters":{"type":"object","properties":{"strict":{"type":"boolean"}},"required":["strict"]}}]}]}}`,
			check: func(t *testing.T, out string) {
				if !gjson.Get(out, "request.tools.0.functionDeclarations.0.parameters.properties.strict").Exists() {
					t.Fatalf("business property removed: %s", out)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := sanitizeAntigravityRequestSchemas(tc.body, false)
			tc.check(t, out)
		})
	}
}

func TestAntigravityResponseSchemaEmptyEnum(t *testing.T) {
	body := `{"request":{"contents":[{"role":"user","parts":[{"text":"x"}]}],"generationConfig":{"responseMimeType":"application/json","responseSchema":{"type":"object","properties":{"slot":{"type":"string","enum":["a","","b"]},"blank":{"type":"string","enum":[""]}}}}}}`
	out := sanitizeAntigravityRequestSchemas(body, false)
	slot := gjson.Get(out, "request.generationConfig.responseSchema.properties.slot.enum")
	for _, v := range slot.Array() {
		if v.String() == "" {
			t.Fatalf("empty enum member survived: %s", slot.Raw)
		}
	}
	if len(slot.Array()) != 2 {
		t.Fatalf("non-empty enum members changed: %s", slot.Raw)
	}
	if gjson.Get(out, "request.generationConfig.responseSchema.properties.blank.enum").Exists() {
		t.Fatalf("all-empty enum should be removed: %s", out)
	}
}
