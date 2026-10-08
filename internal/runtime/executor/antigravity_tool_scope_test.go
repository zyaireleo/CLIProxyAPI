package executor

import (
	"fmt"
	"testing"

	"github.com/tidwall/gjson"
)

func TestAntigravityToolScopeEntryPathsAndHistory(t *testing.T) {
	for _, declarations := range []string{"functionDeclarations", "function_declarations"} {
		for _, parameters := range []string{"parameters", "parametersJsonSchema", "parameters_json_schema"} {
			for _, validated := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/%v", declarations, parameters, validated), func(t *testing.T) {
					payload := fmt.Sprintf(`{"request":{"contents":[{"role":"model","parts":[{"functionCall":{"name":"lookup","args":{"scope":"workspace","query":"x"}}}]},{"role":"user","parts":[{"functionResponse":{"name":"lookup","response":{"scope":"keep"}}}]}],"tools":[{"%s":[{"name":"lookup","%s":{"type":"object","scope":{"type":"string"},"properties":{"scope":{"type":"string"},"query":{"type":"string","scope":"internal"}},"required":["scope","query"]},"response":{"type":"string","scope":"unchanged"}}]}],"generationConfig":{"responseSchema":{"type":"string","scope":"unchanged"}}}}`, declarations, parameters)
					got := sanitizeAntigravityRequestSchemas(payload, validated)
					if gjson.Get(payload, "request.contents").Raw != gjson.Get(got, "request.contents").Raw {
						t.Fatal("history changed")
					}
					outputParameters := parameters
					if outputParameters == "parametersJsonSchema" {
						outputParameters = "parameters"
					}
					base := "request.tools.0." + declarations + ".0."
					schema := gjson.Get(got, base+outputParameters)
					if schema.Get("scope").Exists() || schema.Get("properties.query.scope").Exists() {
						t.Fatal(got)
					}
					if schema.Get("properties.scope.type").String() != "string" {
						t.Fatal(got)
					}
					if gjson.Get(got, base+"response.scope").String() != "unchanged" || gjson.Get(got, "request.generationConfig.responseSchema.scope").String() != "unchanged" {
						t.Fatal("response schema policy changed")
					}
				})
			}
		}
	}
}
