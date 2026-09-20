package proxy

import (
	"net/http"
	"strings"

	"github.com/codex2api/auth"
)

func ResolveCodexOutboundClientIdentity(account *auth.Account, apiKey string, deviceCfg *DeviceProfileConfig, downstreamHeaders http.Header) (userAgent, version, originator string) {
	// Outbound identity is account/configuration owned. A recognized client UA
	// only selects a saved profile in multi mode; it cannot seed or upgrade it.
	userAgent, version, _ = resolveCodexOutboundClientHeaders(account, apiKey, nil, nil)
	settings := CurrentRuntimeSettings()
	if cfg := codexUserAgentConfigFromJSON(settings.CodexUserAgentConfig); cfg.Mode == CodexUserAgentModeMulti {
		floor := ""
		if settings.ClientCompatMode == ClientCompatModeAuto {
			floor = settings.CodexMinCLIVersion
		}
		userAgent, version, _ = codexUserAgentFromProfile(cfg.profile(codexIncomingClientKind(downstreamHeaders)), floor)
	}
	if deviceCfg != nil && strings.TrimSpace(deviceCfg.UserAgent) != "" {
		userAgent = strings.TrimSpace(deviceCfg.UserAgent)
		version = codexOwnedUserAgentVersion(userAgent, version)
	}
	if configured := codexAccountHeader(account, "User-Agent"); configured != "" {
		userAgent = configured
		version = codexOwnedUserAgentVersion(userAgent, version)
	}
	if configured := codexAccountHeader(account, "Version"); configured != "" {
		userAgent = replaceCodexOwnedUserAgentVersion(userAgent, configured)
		version = configured
	}
	originator = CodexOriginatorForGeneratedUserAgent(userAgent)
	if name := codexUserAgentClientName(userAgent); name != "" {
		originator = name
	}
	return userAgent, version, originator
}

// Account-owned custom profiles need the same version/originator consistency as
// generated official profiles, even when their client name is not recognized.
func codexOwnedUserAgentVersion(userAgent, fallback string) string {
	if _, version, ok := parseCodexClientVersionDetails(userAgent); ok {
		return version
	}
	if _, rest, ok := strings.Cut(userAgent, "/"); ok {
		if parts := strings.Fields(rest); len(parts) > 0 {
			return parts[0]
		}
	}
	return fallback
}

func replaceCodexOwnedUserAgentVersion(userAgent, version string) string {
	if _, _, ok := parseCodexClientVersionDetails(userAgent); ok {
		return replaceCodexUserAgentVersion(userAgent, version)
	}
	if name, rest, ok := strings.Cut(userAgent, "/"); ok {
		if parts := strings.Fields(rest); len(parts) > 0 {
			return name + "/" + version + strings.TrimPrefix(rest, parts[0])
		}
	}
	return userAgent
}

func ApplyCodexAccountClientIdentity(headers http.Header, account *auth.Account, apiKey string, config *DeviceProfileConfig, userAgentEnabled bool, incoming ...http.Header) {
	if headers == nil {
		return
	}
	var source http.Header
	if len(incoming) > 0 {
		source = incoming[0]
	}
	ua, version, originator := ResolveCodexOutboundClientIdentity(account, apiKey, config, source)
	appVersionPresent := false
	for name := range headers {
		if strings.EqualFold(name, "X-Codex-App-Version") {
			appVersionPresent = true
		}
	}
	deleteHeaderCaseInsensitive(headers, "X-Codex-App-Version")
	for _, name := range []string{"User-Agent", "Version", "Originator"} {
		deleteHeaderCaseInsensitive(headers, name)
	}
	if userAgentEnabled {
		headers.Set("User-Agent", ua)
		headers.Set("Version", version)
		if appVersionPresent {
			headers.Set("X-Codex-App-Version", version)
		}
	} else {
		headers["User-Agent"] = []string{""}
	}
	headers.Set("Originator", originator)
}

func applyCodexAuxiliaryClientHeaders(request *http.Request, account *auth.Account, apiKey string, deviceCfg *DeviceProfileConfig, downstreamHeaders http.Header, requestedVersion string) {
	if request == nil {
		return
	}
	if request.Header == nil {
		request.Header = make(http.Header)
	}
	userAgent, version, originator := ResolveCodexOutboundClientIdentity(account, apiKey, deviceCfg, downstreamHeaders)
	accountProfile := codexAccountHeader(account, "User-Agent") != "" || codexAccountHeader(account, "Version") != ""
	if requestedVersion = strings.TrimSpace(requestedVersion); requestedVersion != "" && !accountProfile {
		userAgent = replaceCodexOwnedUserAgentVersion(userAgent, requestedVersion)
		version = requestedVersion
	}
	request.Header.Set("User-Agent", userAgent)
	request.Header.Set("Version", version)
	request.Header.Set("Originator", originator)
	RecordUpstreamUserAgent(request.Context(), request.Header.Get("User-Agent"))
}
