package admin

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/codex2api/auth"
	"github.com/gin-gonic/gin"
)

// Reuse the write contract so response-only runtime fields never enter a backup.
// Login credentials and custom rules maintained on their own versioned page
// stay on the target server; importing settings must not replace its admin key.
func portableSettings(snapshot *settingsResponse) (map[string]json.RawMessage, error) {
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		return nil, err
	}
	var values map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &values); err != nil {
		return nil, err
	}
	allowed := make(map[string]bool)
	for _, field := range reflect.VisibleFields(reflect.TypeOf(updateSettingsReq{})) {
		name := strings.Split(field.Tag.Get("json"), ",")[0]
		if name != "" && name != "-" {
			allowed[name] = true
			// Encode empty collections explicitly so importing into a server with
			// existing entries clears them instead of treating null as omitted.
			if string(values[name]) == "null" {
				typ := field.Type
				for typ.Kind() == reflect.Pointer {
					typ = typ.Elem()
				}
				if typ.Kind() == reflect.Slice {
					values[name] = json.RawMessage(`[]`)
				}
				if typ.Kind() == reflect.Map {
					values[name] = json.RawMessage(`{}`)
				}
			}
		}
	}
	for _, name := range []string{"admin_secret", "response_cache_config_generation", "prompt_filter_custom_patterns", "prompt_filter_custom_patterns_expected"} {
		delete(allowed, name)
	}
	for name := range values {
		if !allowed[name] {
			delete(values, name)
		}
	}
	return values, nil
}

// settingsExportSecretFields are the service credentials a backup carries only
// on explicit opt-in. Import treats a missing key as "keep the target's value",
// so a backup without them never clears the target server's credentials.
var settingsExportSecretFields = []string{"github_token", "prompt_filter_review_api_key", "image_s3_access_key", "image_s3_secret_key"}

// settingsExportIncludesSecrets reads the include_secrets opt-in.
func settingsExportIncludesSecrets(c *gin.Context) bool {
	value, _ := strconv.ParseBool(strings.TrimSpace(c.Query("include_secrets")))
	return value
}

// Export only through the authenticated administrator route. With
// include_secrets the file contains service credentials, so neither browser
// nor intermediary may cache it.
func (h *Handler) ExportSettings(c *gin.Context) {
	includeSecrets := settingsExportIncludesSecrets(c)
	snapshot, err := h.settingsSnapshot(c.Request.Context())
	if err != nil {
		writeError(c, http.StatusInternalServerError, err.Error())
		return
	}
	values, err := portableSettings(snapshot)
	if err != nil {
		writeError(c, http.StatusInternalServerError, "导出设置失败")
		return
	}
	stored, err := h.db.GetSystemSettings(c.Request.Context())
	if err != nil {
		writeError(c, http.StatusInternalServerError, "读取设置失败")
		return
	}
	if stored != nil && includeSecrets {
		// These write-only fields are intentionally absent from ordinary GET.
		values["github_token"], _ = json.Marshal(stored.GithubToken)
		values["prompt_filter_review_api_key"], _ = json.Marshal(stored.PromptFilterReviewAPIKey)
	}
	// Ordinary settings expose has_secret instead of OAuth client secrets. A
	// portable backup needs the stored credentials, never deployment env values.
	oauthRaw, err := h.db.LoadAntigravityOAuthConfig(c.Request.Context())
	if err != nil {
		writeError(c, http.StatusInternalServerError, "读取 OAuth 设置失败")
		return
	}
	oauth, err := auth.ParseAntigravityOAuthSettings(oauthRaw)
	if err != nil {
		writeError(c, http.StatusInternalServerError, "OAuth 设置无效，无法导出")
		return
	}
	if oauth.Clients == nil {
		oauth.Clients = []auth.AntigravityOAuthClientConfig{}
	}
	if !includeSecrets {
		// An empty client_secret keeps the target's saved secret for that key.
		for i := range oauth.Clients {
			oauth.Clients[i].ClientSecret = ""
		}
		for _, name := range settingsExportSecretFields {
			delete(values, name)
		}
	}
	values["antigravity_oauth_clients"], _ = json.Marshal(oauth.Clients)
	values["antigravity_oauth_client_key"], _ = json.Marshal(oauth.ActiveKey)
	c.Header("Cache-Control", "no-store")
	c.Header("Content-Disposition", `attachment; filename="codex2api-settings.json"`)
	c.JSON(http.StatusOK, gin.H{
		"format": "codex2api.settings", "version": 1, "exported_at": time.Now().UTC(),
		"secrets_included": includeSecrets, "settings": values,
	})
}
