package proxy

import (
	"net/http"
	"strings"
)

const CodexUserAgentModeMulti = "multi"

// Each client keeps its own saved configuration, including when inactive.
// Nested profiles and mode switches are deliberately absent from this schema.
type CodexUserAgentProfile struct {
	RawUserAgent  string `json:"raw_user_agent,omitempty"`
	ClientName    string `json:"client_name,omitempty"`
	ClientVersion string `json:"client_version,omitempty"`
	OSName        string `json:"os_name,omitempty"`
	OSVersion     string `json:"os_version,omitempty"`
	Arch          string `json:"arch,omitempty"`
	Terminal      string `json:"terminal,omitempty"`
	AppName       string `json:"app_name,omitempty"`
	AppVersion    string `json:"app_version,omitempty"`
}

func codexUserAgentProfile(cfg CodexUserAgentConfig) CodexUserAgentProfile {
	return CodexUserAgentProfile{cfg.RawUserAgent, cfg.ClientName, cfg.ClientVersion, cfg.OSName, cfg.OSVersion, cfg.Arch, cfg.Terminal, cfg.AppName, cfg.AppVersion}
}

func (p CodexUserAgentProfile) config(kind string) CodexUserAgentConfig {
	return CodexUserAgentConfig{ClientKind: kind, RawUserAgent: p.RawUserAgent, ClientName: p.ClientName, ClientVersion: p.ClientVersion, OSName: p.OSName, OSVersion: p.OSVersion, Arch: p.Arch, Terminal: p.Terminal, AppName: p.AppName, AppVersion: p.AppVersion}
}

func (cfg CodexUserAgentConfig) profile(kind CodexClientKind) CodexUserAgentConfig {
	if profile, found := cfg.Profiles[string(kind)]; found {
		return profile.config(string(kind))
	}
	// Older configurations contain only the selected flat profile.
	if effectiveCodexClientKind(cfg) == kind {
		return codexUserAgentProfile(cfg).config(string(kind))
	}
	return CodexUserAgentConfig{ClientKind: string(kind)}
}

// The incoming prefix selects a trusted preset only. Its version, platform,
// terminal and device values never become part of the outgoing profile.
func codexIncomingClientKind(headers http.Header) CodexClientKind {
	name := strings.ToLower(strings.TrimSpace(codexUserAgentClientName(headers.Get("User-Agent"))))
	switch name {
	case "codex-tui", "codex_cli_rs":
		return CodexClientKindTUI
	case "codex desktop", "codex_app", "codex_chatgpt_desktop", "codex_work_desktop", "codex_atlas":
		return CodexClientKindDesktop
	case "codex_vscode":
		return CodexClientKindVSCode
	case "codex_exec":
		return CodexClientKindExec
	default:
		return CodexClientKindCustom
	}
}
