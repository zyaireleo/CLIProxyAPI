package pluginhost

import (
	"reflect"
	"testing"
)

func TestLegacyQuotaPluginsPreservesExplicitEmptyCoverage(t *testing.T) {
	if got := legacyQuotaPlugins([]string{}); len(got) != 0 {
		t.Fatalf("empty coverage claimed plugin owners: %v", got)
	}
	if got := legacyQuotaPlugins(nil); !reflect.DeepEqual(got, []string{"antigravity-priority", "codex-quota-scheduler"}) {
		t.Fatalf("default coverage=%v", got)
	}
	if got := legacyQuotaPlugins([]string{"antigravity"}); !reflect.DeepEqual(got, []string{"antigravity-priority"}) {
		t.Fatalf("antigravity-only coverage=%v", got)
	}
}
