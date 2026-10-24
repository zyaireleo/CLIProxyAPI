package management

import (
	"net/http"
	"strings"
	"testing"

	"golang.org/x/net/html"
)

func TestNativeQuotaUIShellHasNoSecretsAndStrictCSP(t *testing.T) {
	h, router := newNativeQuotaFixture(t)
	h.cfg.RemoteManagement.SecretKey = "server-side-secret-never-in-html"
	response := nativeQuotaRequest(router, http.MethodGet, "/native-quota.html?key=do-not-embed-query", "", "Authorization")
	if response.Code != http.StatusOK || response.Body.String() != nativeQuotaHTML {
		t.Fatalf("static shell failed: %d", response.Code)
	}
	for _, secret := range []string{nativeQuotaTestKey, h.cfg.RemoteManagement.SecretKey, "do-not-embed-query"} {
		if strings.Contains(response.Body.String(), secret) {
			t.Fatal("request/server secret embedded in shell")
		}
	}
	csp := response.Header().Get("Content-Security-Policy")
	for _, required := range []string{"default-src 'none'", "connect-src 'self'", "frame-ancestors 'none'", nativeQuotaInlineHash("script"), nativeQuotaInlineHash("style")} {
		if !strings.Contains(csp, required) {
			t.Errorf("missing CSP directive %s", required)
		}
	}
	if strings.Contains(csp, "unsafe-inline") || response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("unsafe shell headers: %v", response.Header())
	}
	h.cfg.RemoteManagement.DisableControlPanel = true
	response = nativeQuotaRequest(router, http.MethodGet, "/native-quota.html", "", "")
	if response.Code != http.StatusNotFound {
		t.Fatal("disabled control panel still served")
	}
}

func TestNativeQuotaUIStaticDOMSafety(t *testing.T) {
	for _, forbidden := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "localStorage", "sessionStorage", "indexedDB", "document.cookie", "console.", "location.search", "location.hash", "animation:", "transition:"} {
		if strings.Contains(nativeQuotaHTML, forbidden) {
			t.Errorf("unsafe or unwanted UI primitive: %s", forbidden)
		}
	}
	for _, required := range []string{"textContent", "replaceChildren", "document.visibilityState === 'visible'", "el('auto').checked", "30000", "credentials: 'omit'", "redirect: 'error'", "cache: 'no-store'", "referrerPolicy: 'no-referrer'", ":focus-visible", "window.addEventListener('pagehide', forget)"} {
		if !strings.Contains(nativeQuotaHTML, required) {
			t.Errorf("missing UI safety behavior: %s", required)
		}
	}
	root, errParse := html.Parse(strings.NewReader(nativeQuotaHTML))
	if errParse != nil {
		t.Fatal(errParse)
	}
	ids := make(map[string]bool)
	scripts, styles, captions, passwordInputs := 0, 0, 0, 0
	var walk func(*html.Node)
	walk = func(node *html.Node) {
		if node.Type == html.ElementNode {
			switch node.Data {
			case "script":
				scripts++
			case "style":
				styles++
			case "caption":
				captions++
			}
			for _, attribute := range node.Attr {
				switch attribute.Key {
				case "src", "srcset", "href", "action", "formaction":
					t.Errorf("unexpected resource/navigation attribute: %s=%s", attribute.Key, attribute.Val)
				case "id":
					if ids[attribute.Val] {
						t.Errorf("duplicate element ID: %s", attribute.Val)
					}
					ids[attribute.Val] = true
				case "type":
					if node.Data == "input" && attribute.Val == "password" {
						passwordInputs++
					}
				case "checked":
					t.Error("auto refresh must be opt in")
				}
				if strings.HasPrefix(attribute.Key, "on") {
					t.Errorf("inline handler %s is forbidden", attribute.Key)
				}
			}
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(root)
	if scripts != 1 || styles != 1 || captions != 1 || passwordInputs != 1 {
		t.Fatalf("unexpected shell DOM: scripts=%d styles=%d captions=%d passwordInputs=%d", scripts, styles, captions, passwordInputs)
	}
	for _, id := range []string{"accounts", "key", "auto", "notice", "config", "migration", "save-config", "preview"} {
		if !ids[id] {
			t.Errorf("missing essential control %s", id)
		}
	}
}
