package util

import (
	"strconv"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// removeAntigravityToolScope visits schema positions only. In particular, names
// in properties/$defs and values in defaults/examples are not schema keywords.
// Run before malformed-schema repair so object metadata cannot become a property.
func removeAntigravityToolScope(schema string) string {
	if !gjson.Valid(schema) {
		return schema
	}
	root := gjson.Parse(schema)
	path := ""
	if root.IsObject() && len(root.Map()) == 1 && root.Get("schema").IsObject() {
		root, path = root.Get("schema"), "schema"
	}
	var paths []string
	var visit func(gjson.Result, string)
	visit = func(node gjson.Result, path string) {
		if !node.IsObject() {
			return
		}
		node.ForEach(func(key, value gjson.Result) bool {
			child := joinPath(path, escapeGJSONPathKey(key.String()))
			switch key.String() {
			case "scope":
				paths = append(paths, child)
			case "properties", "patternProperties", "$defs", "definitions", "dependentSchemas", "dependencies":
				if value.IsObject() {
					value.ForEach(func(name, subschema gjson.Result) bool {
						visit(subschema, joinPath(child, escapeGJSONPathKey(name.String())))
						return true
					})
				}
			case "items", "prefixItems", "allOf", "anyOf", "oneOf":
				if value.IsArray() {
					for i, subschema := range value.Array() {
						visit(subschema, joinPath(child, strconv.Itoa(i)))
					}
				} else {
					visit(value, child)
				}
			case "additionalProperties", "additionalItems", "unevaluatedProperties", "unevaluatedItems", "contains", "propertyNames", "not", "if", "then", "else", "contentSchema":
				visit(value, child)
			}
			return true
		})
	}
	visit(root, path)
	for _, path := range paths {
		updated, err := sjson.Delete(schema, path)
		if err != nil {
			return schema
		}
		schema = updated
	}
	return schema
}
