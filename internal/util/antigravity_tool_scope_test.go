package util

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/tidwall/gjson"
)

func TestAntigravityToolScopeSchemaPositions(t *testing.T) {
	for _, metadata := range []string{`"internal"`, `{"type":"string"}`, `["internal"]`, `null`, `false`, `42`} {
		for _, wrapper := range []string{"root", "properties", "items", "prefixItems", "allOf", "anyOf", "oneOf", "$defs", "definitions", "dependentSchemas", "dependencies", "additionalProperties", "if", "then", "else", "not", "contains", "schema"} {
			t.Run(wrapper+"/"+metadata, func(t *testing.T) {
				leaf := `{"type":"string","scope":` + metadata + `}`
				input, path := leaf, ""
				switch wrapper {
				case "root":
				case "properties", "$defs", "definitions", "dependentSchemas", "dependencies":
					input = `{"` + wrapper + `":{"scope":` + leaf + `}}`
					path = gjson.Escape(wrapper) + ".scope"
				case "prefixItems", "allOf", "anyOf", "oneOf":
					input = `{"` + wrapper + `":[` + leaf + `]}`
					path = wrapper + ".0"
				default:
					input = `{"` + wrapper + `":` + leaf + `}`
					path = wrapper
				}
				got := removeAntigravityToolScope(input)
				node := gjson.Parse(got)
				if path != "" {
					node = gjson.Get(got, path)
				}
				if node.Get("scope").Exists() || node.Get("type").String() != "string" {
					t.Fatalf("incorrect cleanup: %s", got)
				}
				if second := removeAntigravityToolScope(got); second != got {
					t.Fatalf("not idempotent: %s", second)
				}
			})
		}
	}
}

func TestAntigravityToolScopePreservesDataAndNames(t *testing.T) {
	input := `{"type":"object","scope":{"internal":true},"properties":{"scope":{"type":"string"},"a.b*?":{"type":"string","scope":false},"properties":{"type":"object","scope":"drop","properties":{"scope":{"type":"string"}}}},"required":["scope"],"default":{"scope":9007199254740993},"example":{"scope":"<keep>"},"examples":[{"scope":"keep"}],"const":{"scope":true},"enum":[{"scope":"keep"}],"description":"scope is a business parameter","x-data":{"scope":"keep"}}`
	got := removeAntigravityToolScope(input)
	for _, path := range []string{"properties.scope", "properties.properties.properties.scope", "required", "default", "example", "examples", "const", "enum", "description", "x-data"} {
		if gjson.Get(input, path).Raw != gjson.Get(got, path).Raw {
			t.Errorf("data changed at %s", path)
		}
	}
	if gjson.Get(got, `properties.a\.b\*\?.scope`).Exists() || gjson.Get(got, "properties.properties.scope").Exists() {
		t.Fatal(got)
	}
	for _, input := range []string{`{"type":"string"}`, `true`, `{"broken"`} {
		if removeAntigravityToolScope(input) != input {
			t.Fatalf("no-op changed %s", input)
		}
	}
}

func TestAntigravityToolScopeRunsBeforeRepair(t *testing.T) {
	input := `{"type":"object","scope":{"type":"string"},"properties":{"scope":{"type":"string"},"query":{"type":"string","scope":"internal"}},"required":["scope","query"]}`
	for _, placeholder := range []bool{false, true} {
		got := CleanJSONSchemaForAntigravityTool(input, placeholder)
		if gjson.Get(got, "scope").Exists() || gjson.Get(got, "properties.query.scope").Exists() {
			t.Fatal(got)
		}
		if gjson.Get(got, "properties.scope.type").String() != "string" {
			t.Fatal(got)
		}
		var first, second any
		_ = json.Unmarshal([]byte(got), &first)
		_ = json.Unmarshal([]byte(CleanJSONSchemaForAntigravityTool(got, placeholder)), &second)
		if !reflect.DeepEqual(first, second) {
			t.Fatal("cleanup changed on second pass")
		}
		baseline := cleanAntigravityToolSchema(input, placeholder)
		if CleanJSONSchemaForAntigravityFunctionResponse(input, placeholder) != baseline {
			t.Fatal("result policy changed")
		}
	}
	if !gjson.Get(CleanJSONSchemaForGemini(`{"type":"string","scope":"keep"}`), "scope").Exists() {
		t.Fatal("Gemini policy changed")
	}
	if !gjson.Get(CleanJSONSchemaForAntigravityResponse(`{"type":"string","scope":"keep"}`), "scope").Exists() {
		t.Fatal("response policy changed")
	}
}
