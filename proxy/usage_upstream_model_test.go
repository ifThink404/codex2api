package proxy

import (
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
)

// 网关合成的信封里 model 写的是请求模型。把它记进 upstream_response_model 就是
// 「从请求反推」，这一列明令禁止：宁可留空，也不能显示成「上游确认用了请求模型」。
func TestClearSynthesizedUpstreamResponseModel(t *testing.T) {
	antigravityOAuth := &auth.Account{
		DBID: 1, UpstreamType: auth.UpstreamAntigravity,
		AccessToken: "ag-token", AntigravityProjectID: "project",
	}
	antigravityAPIKey := &auth.Account{
		DBID: 2, UpstreamType: auth.UpstreamAntigravity, APIKey: "ag-key",
	}
	official := &auth.Account{DBID: 3, AccessToken: "official-token", PlanType: "pro"}
	relay := &auth.Account{
		DBID: 4, UpstreamType: auth.UpstreamOpenAIResponses,
		BaseURL: "https://relay.example/v1", APIKey: "relay-key",
	}
	claude := &auth.Account{DBID: 5, UpstreamType: auth.UpstreamClaude, AccessToken: "claude-token"}

	if got := antigravityOAuth.AntigravityAuthKind(); got != auth.AntigravityAuthKindOAuth {
		t.Fatalf("fixture auth kind = %q, want OAuth（判据本身错了后面全白测）", got)
	}
	if got := antigravityAPIKey.AntigravityAuthKind(); got != auth.AntigravityAuthKindAPIKey {
		t.Fatalf("fixture auth kind = %q, want api_key", got)
	}

	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1})
	t.Cleanup(store.Stop)
	for _, account := range []*auth.Account{antigravityOAuth, antigravityAPIKey, official, relay, claude} {
		store.AddAccount(account)
	}
	handler := NewHandler(store, nil, nil, nil)

	for _, tc := range []struct {
		name      string
		accountID int64
		want      string
	}{
		{"Antigravity OAuth 抹掉合成值", 1, ""},
		{"Antigravity API Key 是真声明，保留", 2, "observed-model"},
		{"官方 Codex 保留", 3, "observed-model"},
		{"中转保留", 4, "observed-model"},
		{"Claude 保留", 5, "observed-model"},
		{"账号不在池中时保留", 999, "observed-model"},
		{"没有账号归属时保留", 0, "observed-model"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := &database.UsageLogInput{AccountID: tc.accountID, UpstreamResponseModel: "observed-model"}
			handler.clearSynthesizedUpstreamResponseModel(input)
			if input.UpstreamResponseModel != tc.want {
				t.Fatalf("UpstreamResponseModel = %q, want %q", input.UpstreamResponseModel, tc.want)
			}
		})
	}

	// 空值与无号池时不得 panic，也不得凭空造值。
	empty := &database.UsageLogInput{AccountID: 1}
	handler.clearSynthesizedUpstreamResponseModel(empty)
	if empty.UpstreamResponseModel != "" {
		t.Fatalf("空值被改写成 %q", empty.UpstreamResponseModel)
	}
	kept := &database.UsageLogInput{AccountID: 1, UpstreamResponseModel: "observed-model"}
	(*Handler)(nil).clearSynthesizedUpstreamResponseModel(kept)
	if kept.UpstreamResponseModel != "observed-model" {
		t.Fatalf("无号池时不该改写，got %q", kept.UpstreamResponseModel)
	}
}
