package admin

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/proxy"
	"github.com/codex2api/security/promptfilter"
	"github.com/gin-gonic/gin"
)

func TestBuiltinRuleEditingPersistsAndPreservesOtherSettings(t *testing.T) {
	db := newTestAdminDB(t)
	tc := cache.NewMemory(4)
	t.Cleanup(func() { _ = tc.Close() })
	settings := defaultBootstrapSettings()
	settings.PromptFilterDisabledPatterns = `["prompt_fake_authorization"]`
	ruleRequireNoError(t, db.UpdateSystemSettings(t.Context(), settings))
	store := auth.NewStore(db, tc, settings)
	t.Cleanup(store.Stop)
	h := NewHandler(store, db, tc, proxy.NewRateLimiter(settings.GlobalRPM), "admin-secret")
	var original promptfilter.BuiltinPatternOverride
	for _, pattern := range promptfilter.BuiltinPatternConfigs() {
		if pattern.Name == "prompt_fake_authorization" {
			original = promptfilter.BuiltinPatternFields(pattern)
		}
	}
	ruleRequireNotEmpty(t, original.Name)
	send := func(expected *promptfilter.BuiltinPatternOverride, edit *promptfilter.BuiltinPatternOverride) *httptest.ResponseRecorder {
		body, err := json.Marshal(map[string]any{"expected": expected, "rule": edit})
		ruleRequireNoError(t, err)
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPut, "/prompt-filter/rules/builtin/"+original.Name, bytes.NewReader(body))
		c.Params = gin.Params{{Key: "name", Value: original.Name}}
		h.UpdatePromptFilterBuiltinRule(c)
		return w
	}
	edit := original
	edit.Pattern, edit.Weight = "builtin_override_probe_987654", 83
	signalOnly, minimum := true, 2
	edit.SignalOnly, edit.MinMatches = &signalOnly, &minimum
	edit.AllPatterns = []string{"required_probe"}
	edit.AnyPatterns = []string{"alternative_alpha", "alternative_beta"}
	edit.ExcludePatterns = []string{"excluded_probe"}
	edit.AuthorizationExcludePatterns = []string{"authorized_probe"}
	w := send(&original, &edit)
	ruleRequireEqual(t, 200, w.Code, w.Body.String())
	var rules promptFilterRulesResponse
	ruleRequireNoError(t, json.Unmarshal(w.Body.Bytes(), &rules))
	for _, rule := range rules.BuiltinPatterns {
		if rule.Name == original.Name {
			ruleRequireTrue(t, rule.Overridden)
			ruleRequireFalse(t, rule.Enabled)
			ruleRequireEqual(t, original, *rule.Default)
			ruleRequireEqual(t, edit.Pattern, rule.Pattern)
			ruleRequireTrue(t, rule.SignalOnly)
			ruleRequireEqual(t, edit.AllPatterns, rule.AllPatterns)
			ruleRequireEqual(t, edit.AnyPatterns, rule.AnyPatterns)
			ruleRequireEqual(t, edit.ExcludePatterns, rule.ExcludePatterns)
			ruleRequireEqual(t, edit.AuthorizationExcludePatterns, rule.AuthorizationExcludePatterns)
			ruleRequireEqual(t, 2, rule.MinMatches)
		}
	}
	ruleRequireEqual(t, []promptfilter.BuiltinPatternOverride{edit}, store.GetPromptFilterConfig().BuiltinOverrides)
	// A stale editor cannot overwrite this change, including by restoring defaults.
	ruleRequireEqual(t, 409, send(&original, nil).Code)
	staleConditions := edit
	staleConditions.ExcludePatterns = []string{"stale_exclusion"}
	ruleRequireEqual(t, 409, send(&staleConditions, nil).Code)
	invalid := edit
	invalid.Pattern = "["
	ruleRequireEqual(t, 400, send(&edit, &invalid).Code)
	invalid = edit
	invalid.AuthorizationExcludePatterns = []string{"["}
	ruleRequireEqual(t, 400, send(&edit, &invalid).Code)
	invalid = edit
	invalid.Name = "renamed"
	ruleRequireEqual(t, 400, send(&edit, &invalid).Code)
	ruleRequireEqual(t, 400, send(nil, &edit).Code)
	// Normal settings persistence has no write access to the overrides column.
	ruleRequireNoError(t, db.UpdateSystemSettings(t.Context(), settings))
	persisted, err := db.GetSystemSettings(t.Context())
	ruleRequireNoError(t, err)
	reloaded := auth.NewStore(nil, nil, persisted)
	t.Cleanup(reloaded.Stop)
	ruleRequireEqual(t, []promptfilter.BuiltinPatternOverride{edit}, reloaded.GetPromptFilterConfig().BuiltinOverrides)
	// Exercise the normal settings handler's runtime publication too.
	settingsRequest, _ := gin.CreateTestContext(httptest.NewRecorder())
	settingsRequest.Request = httptest.NewRequest(http.MethodPut, "/settings", bytes.NewBufferString(`{"prompt_filter_threshold":61}`))
	h.UpdateSettings(settingsRequest)
	ruleRequireEqual(t, 200, settingsRequest.Writer.Status())
	ruleRequireEqual(t, []promptfilter.BuiltinPatternOverride{edit}, store.GetPromptFilterConfig().BuiltinOverrides)
	w = send(&edit, nil)
	ruleRequireEqual(t, 200, w.Code, w.Body.String())
	ruleRequireEmpty(t, store.GetPromptFilterConfig().BuiltinOverrides)
	ruleRequireNoError(t, json.Unmarshal(w.Body.Bytes(), &rules))
	for _, rule := range rules.BuiltinPatterns {
		if rule.Name == original.Name {
			ruleRequireFalse(t, rule.Overridden)
			ruleRequireFalse(t, rule.Enabled)
			ruleRequireEqual(t, original.Pattern, rule.Pattern)
			ruleRequireFalse(t, rule.SignalOnly)
			ruleRequireEmpty(t, rule.AllPatterns)
			ruleRequireEmpty(t, rule.AnyPatterns)
			ruleRequireEmpty(t, rule.ExcludePatterns)
			ruleRequireEmpty(t, rule.AuthorizationExcludePatterns)
			ruleRequireZero(t, rule.MinMatches)
		}
	}
}
