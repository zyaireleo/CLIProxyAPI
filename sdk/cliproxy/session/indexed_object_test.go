package session

import (
	"github.com/tidwall/gjson"
	"testing"
)

func TestIndexedSessionObjectPreservesLookup(t *testing.T) {
	for _, body := range []string{
		`{"contents":[{"parts":[{"text":"large unrelated value"}]}],"session_id":"first","session_id":"second","metadata":{"parent_session_id":"p"}}`,
		`{"metadata":null,"request":{"session_id":"nested"},"contents":[]}`,
		`{"metadata":{"parent_session_id":"first","parent_session_id":"second"},"parentSessionId":"p"}`,
	} {
		root := gjson.Parse(body)
		indexed := indexSessionObject(root)
		for _, path := range []string{"session_id", "metadata.parent_session_id", "parentSessionId", "missing", "request.session_id", "contents"} {
			got, want := indexed.Get(path), root.Get(path)
			if got.Raw != want.Raw || got.Type != want.Type || got.String() != want.String() {
				t.Fatalf("%s: got %v want %v", path, got, want)
			}
		}
	}
}
