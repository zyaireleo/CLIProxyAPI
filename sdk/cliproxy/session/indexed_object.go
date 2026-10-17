package session

import (
	"github.com/tidwall/gjson"
	"strings"
)

// indexedSessionObject scans each top-level value once. Repeated optional
// identity lookups must not rescan large contents/messages or inline images.
type indexedSessionObject struct {
	gjson.Result
	fields map[string]gjson.Result
}

func indexSessionObject(root gjson.Result) indexedSessionObject {
	obj := indexedSessionObject{Result: root}
	if !root.IsObject() {
		return obj
	}
	obj.fields = make(map[string]gjson.Result)
	root.ForEach(func(key, value gjson.Result) bool {
		name := key.String()
		if _, exists := obj.fields[name]; !exists {
			obj.fields[name] = value
		}
		return true
	})
	return obj
}
func (o indexedSessionObject) Get(path string) gjson.Result {
	if o.fields == nil {
		return o.Result.Get(path)
	}
	key, rest, nested := strings.Cut(path, ".")
	value := o.fields[key]
	if nested {
		return value.Get(rest)
	}
	return value
}
