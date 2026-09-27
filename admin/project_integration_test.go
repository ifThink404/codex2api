package admin

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func TestProjectIntegrationSignsWithExistingBinding(t *testing.T) {
	db := newTestAdminDB(t)
	keyID := insertTestAPIKey(t, db, "project binding")
	secret := strings.Repeat("p", 32)
	if err := db.CreatePromptFilterNewAPIBinding(context.Background(), &database.PromptFilterNewAPIBinding{APIKeyID: keyID, PlatformCode: "project-test", PlatformName: "test", Secret: secret, Enabled: true, PromptFilterScope: database.PromptFilterScopeLocalOnly}); err != nil {
		t.Fatal(err)
	}
	key, err := db.GetAPIKeyByID(context.Background(), keyID)
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{db: db}
	gin.SetMode(gin.TestMode)
	requests := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Path != "/prefix/api/integration/codex2api/summary" {
			t.Error(r.URL.Path)
		}
		fields := make([]string, 0, 5)
		for _, n := range []string{"Timestamp", "Nonce", "Platform", "Key-Fingerprint", "Day-Start"} {
			fields = append(fields, r.Header.Get("X-CPA-"+n))
		}
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write([]byte("project-summary-v1\nGET\n/api/integration/codex2api/summary\n" + strings.Join(fields, "\n")))
		if hex.EncodeToString(mac.Sum(nil)) != r.Header.Get("X-CPA-Signature") {
			t.Error("invalid signature")
		}
		digest := sha256.Sum256([]byte(key.Key))
		if fields[2] != "project-test" || fields[3] != hex.EncodeToString(digest[:]) {
			t.Error("wrong binding identity")
		}
		if strings.Contains(fmt.Sprint(r.Header), key.Key) || strings.Contains(fmt.Sprint(r.Header), secret) {
			t.Error("secret leaked")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"version":1,"rpm":8,"channels":[]}`))
	}))
	defer upstream.Close()
	do := func(base string, id int64) *httptest.ResponseRecorder {
		b, _ := json.Marshal(map[string]any{"base_url": base, "api_key_id": id, "day_start": time.Now().Unix() - 100})
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest("POST", "/api/admin/project-integration/newapi/summary", strings.NewReader(string(b)))
		c.Request.Header.Set("Content-Type", "application/json")
		h.ProjectNewAPISummary(c)
		return w
	}
	if w := do(upstream.URL+"/prefix", keyID); w.Code != 200 || !strings.Contains(w.Body.String(), `"rpm":8`) {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := do(upstream.URL, 99999); w.Code != 409 {
		t.Fatal("missing binding not rejected", w.Code)
	}
	if w := do("file:///etc/passwd", keyID); w.Code != 400 {
		t.Fatal("non-HTTP URL accepted", w.Code)
	}
	if requests != 1 {
		t.Fatal("invalid binding called upstream")
	}
	leaked := false
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked = true }))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 302) }))
	defer redirect.Close()
	if w := do(redirect.URL, keyID); w.Code != 502 || leaked {
		t.Fatal("followed signed redirect", w.Code)
	}
}
