package security

import (
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// inlineScriptPattern matches <script> elements without a src attribute; the
// browser hashes their exact text content for CSP.
var inlineScriptPattern = regexp.MustCompile(`(?s)<script(\s[^>]*)?>(.*?)</script>`)

// Every inline script in frontend/index.html must be allowed by the CSP
// script-src hash; otherwise the browser blocks it (the theme restore script
// drifted once when the script changed but the hash did not).
func TestCSPAllowsIndexInlineScripts(t *testing.T) {
	html, err := os.ReadFile("../frontend/index.html")
	if err != nil {
		t.Fatal(err)
	}
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(SecurityHeadersMiddleware())
	r.GET("/", func(c *gin.Context) { c.String(http.StatusOK, "OK") })
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	csp := w.Header().Get("Content-Security-Policy")

	var scriptSrc string
	for _, directive := range strings.Split(csp, ";") {
		if fields := strings.Fields(directive); len(fields) > 0 && fields[0] == "script-src" {
			scriptSrc = " " + strings.Join(fields[1:], " ") + " "
		}
	}
	if scriptSrc == "" {
		t.Fatalf("CSP has no script-src directive: %q", csp)
	}
	inline := 0
	for _, match := range inlineScriptPattern.FindAllStringSubmatch(string(html), -1) {
		if strings.Contains(match[1], "src=") {
			continue
		}
		inline++
		sum := sha256.Sum256([]byte(match[2]))
		source := "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
		if !strings.Contains(scriptSrc, " "+source+" ") {
			t.Errorf("inline script #%d of frontend/index.html is blocked: script-src lacks %s", inline, source)
		}
	}
	if inline == 0 {
		t.Fatal("expected the theme restore inline script in frontend/index.html")
	}
}
