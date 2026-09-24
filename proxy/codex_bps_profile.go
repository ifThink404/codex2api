package proxy

import (
	"net/http"
	"strings"

	"github.com/codex2api/auth"
)

// Selected once at the request boundary and shared by body, headers and uploads.
type bpsProfileConfig struct {
	profile      auth.CodexBPSProfile
	toolsVersion string
}

func bpsProfile(profile auth.CodexBPSProfile) bpsProfileConfig {
	profile = auth.NormalizeCodexBPSProfile(string(profile))
	version := bpsToolsVersion
	switch profile {
	case auth.BPSExcel:
		version = "tools-excel-core-2026-06-16-3af59f22"
	case auth.BPSSheets:
		version = "tools-sheets-core-2026-06-01-34ba0624"
	case auth.BPSPowerPoint:
		version = "tools-powerpoint-core-2026-08-10-2b886486"
	}
	return bpsProfileConfig{profile: profile, toolsVersion: version}
}

func (p bpsProfileConfig) applyHeaders(headers http.Header) {
	values := map[string]string{
		"Client-Agent-Profile": "document", "Client-Editor": "word", "Client-Host": "Word",
		"Client-Platform": "word", "Client-Platform-Class": "desktop", "Client-Product": "basispoints-word-plugin",
		"Client-Runtime": "officejs", "Office-Host": "Word", "Office-Host-Version": "16.113",
		"Office-Platform": "Mac", "Tools-Version-Id": p.toolsVersion,
	}
	if p.profile != auth.BPSWord {
		name := string(p.profile)
		values["Client-Agent-Profile"], values["Client-Editor"], values["Client-Platform"] = name, name, name
		values["Client-Product"] = "basispoints-" + name + "-plugin"
		values["Client-Host"], values["Client-Runtime"], values["Client-Platform-Class"] = "office", "desktop", "Mac"
		values["Office-Host"] = map[auth.CodexBPSProfile]string{auth.BPSExcel: "Excel", auth.BPSPowerPoint: "PowerPoint"}[p.profile]
		if p.profile == auth.BPSSheets {
			values["Client-Host"], values["Client-Runtime"], values["Client-Platform-Class"] = "apps_script", "web", "web"
			for _, key := range []string{"Office-Host", "Office-Host-Version", "Office-Platform"} {
				delete(values, key)
				headers.Del("X-Openai-Internal-Basispoints-" + key)
			}
		}
	}
	for key, value := range values {
		headers.Set("X-Openai-Internal-Basispoints-"+key, value)
	}
}

func (p bpsProfileConfig) runtimeInstructions() string {
	// Preserve the established Word prompt. Other editors reuse it, with their
	// native tools and host added. Caller-declared tools remain authoritative.
	if p.profile == auth.BPSWord {
		return bpsCallerRuntimeInstructions
	}
	name, tools := "", ""
	switch p.profile {
	case auth.BPSExcel:
		name, tools = "Microsoft Excel", "read_ranges, write_range, chart, pivot_table, table, update_sheet, update_workbook, run_officejs"
	case auth.BPSSheets:
		name, tools = "Google Sheets", "read_ranges, write_range, update_sheet, update_workbook"
	case auth.BPSPowerPoint:
		name, tools = "Microsoft PowerPoint", "read_slides, read_slide_text, read_slide_image, edit_slide_text, edit_slide_ooxml, edit_slide_master, insert_slides_from_html, verify_slides"
	}
	prompt := strings.Replace(bpsCallerRuntimeInstructions, "Microsoft Word, Excel, PowerPoint or any Office application", "Microsoft Word, Excel, PowerPoint, Google Sheets or any Office application", 1)
	return prompt + "\nHost adaptation: the " + name + " preset does not provide a live editor, workbook, presentation, Apps Script or Office runtime to this caller. Do not depend on its built-in tools (" + tools + ") or claim to inspect its files. Tools explicitly declared by the calling application are an exception, even if their names overlap: use their exact schemas and return their call IDs and arguments unchanged. Respond directly to general text and coding requests; do not require a document, selection, sheet or slide."
}

func bpsProfileCacheKey(accountID, cacheKey string, p bpsProfileConfig) string {
	// Retain existing Word session/task IDs. Other profiles cannot reuse them.
	if p.profile == auth.BPSWord {
		return codexIdentityDigest("bps-prompt-cache-v1", accountID, cacheKey)
	}
	return codexIdentityDigest("bps-prompt-cache-profile-v1", accountID, string(p.profile), cacheKey)
}
