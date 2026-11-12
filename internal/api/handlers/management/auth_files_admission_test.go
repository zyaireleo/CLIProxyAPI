package management

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/MoeForge/moego/ojson"
	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestListCredentialsAdmissionPausesV8OnlyAndPagination(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	now := time.Now().UTC()
	m := coreauth.NewManager(nil, nil, nil)
	registerAuthForLookupTest(t, m, &coreauth.Auth{ID: "paused", Index: "paused-index", Provider: "antigravity", Status: coreauth.StatusActive,
		Attributes: map[string]string{"runtime_only": "true"}, AdmissionBackoffs: map[string]coreauth.AdmissionBackoff{
			"gemini-2.5-flash": {Until: now.Add(time.Hour), Level: 3},
		}})
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, m)
	for _, v8 := range []bool{false, true} {
		for _, query := range []string{"", "?page=1&page_size=1"} {
			rec := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(rec)
			ctx.Request = httptest.NewRequest(http.MethodGet, "/credentials"+query, nil)
			ctx.Set(ConfigV8ContextKey, v8)
			h.ListAuthFiles(ctx)
			if rec.Code != http.StatusOK {
				t.Fatalf("credentials status=%d", rec.Code)
			}
			file := ojson.GetBytes(rec.Body.Bytes(), "files.0")
			pauses := file.Get("admission_pauses")
			if !v8 {
				if pauses.Exists() {
					t.Fatal("deprecated v0 schema changed")
				}
				continue
			}
			if !pauses.IsArray() || len(pauses.Array()) != 1 || pauses.Get("0.model").String() != "gemini-2.5-flash" || pauses.Get("0.reason").String() != "local_admission_backoff" {
				t.Fatalf("missing v8 admission diagnosis: %s", pauses.Raw)
			}
			if file.Get("unavailable").Bool() || file.Get("quota_blocked").Bool() || file.Get("status").String() != "active" {
				t.Fatal("diagnostics changed credential health")
			}
		}
	}
}
