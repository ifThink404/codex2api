package admin

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/codex2api/database"
)

func TestRunRefreshAllModels_PartialFailureKeepsOtherChannels(t *testing.T) {
	h := &Handler{modelRefreshFuncs: map[string]channelModelRefreshFunc{
		database.UpstreamChannelGrok: func(ctx context.Context) channelModelRefreshResult {
			return channelModelRefreshResult{Refreshed: 1, Added: []string{"grok-5"}}
		},
		database.UpstreamChannelCodex: func(ctx context.Context) channelModelRefreshResult {
			return channelModelRefreshResult{Error: "官方模型页同步失败: boom", Failed: 1}
		},
		database.UpstreamChannelClaude: func(ctx context.Context) channelModelRefreshResult {
			panic("claude exploded")
		},
	}}
	resp := h.runRefreshAllModels(context.Background())

	if len(resp.Channels) != 3 {
		t.Fatalf("channels = %d, want 3: %+v", len(resp.Channels), resp.Channels)
	}
	// 固定顺序：codex → claude → grok
	if resp.Channels[0].Channel != database.UpstreamChannelCodex || resp.Channels[1].Channel != database.UpstreamChannelClaude || resp.Channels[2].Channel != database.UpstreamChannelGrok {
		t.Fatalf("unexpected channel order: %+v", resp.Channels)
	}
	if resp.Channels[0].Error == "" || resp.Channels[0].Failed != 1 {
		t.Fatalf("codex failure should be reported: %+v", resp.Channels[0])
	}
	if resp.Channels[1].Error == "" || resp.Channels[1].Error[:5] != "panic" {
		t.Fatalf("claude panic should be captured as error: %+v", resp.Channels[1])
	}
	if resp.Channels[2].Refreshed != 1 || len(resp.Channels[2].Added) != 1 {
		t.Fatalf("grok result lost: %+v", resp.Channels[2])
	}
	if len(resp.Added) != 1 || resp.Added[0] != "grok-5" {
		t.Fatalf("aggregated added = %v", resp.Added)
	}
	for _, ch := range resp.Channels {
		if ch.Added == nil {
			t.Fatalf("added must serialize as [] not null: %+v", ch)
		}
	}
}

func TestRunRefreshAllModels_TimeoutIsReportedPerChannel(t *testing.T) {
	h := &Handler{modelRefreshFuncs: map[string]channelModelRefreshFunc{
		database.UpstreamChannelAntigravity: func(ctx context.Context) channelModelRefreshResult {
			<-ctx.Done()
			return channelModelRefreshResult{}
		},
		database.UpstreamChannelCodex: func(ctx context.Context) channelModelRefreshResult {
			return channelModelRefreshResult{Refreshed: 1}
		},
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	resp := h.runRefreshAllModels(ctx)
	if time.Since(started) > 2*time.Second {
		t.Fatalf("refresh did not honour context deadline")
	}
	if resp.Channels[1].Channel != database.UpstreamChannelAntigravity || !errors.Is(context.DeadlineExceeded, context.DeadlineExceeded) || resp.Channels[1].Error != context.DeadlineExceeded.Error() {
		t.Fatalf("antigravity timeout should be reported: %+v", resp.Channels)
	}
	if resp.Channels[0].Error != "" {
		t.Fatalf("codex should not inherit the timeout error: %+v", resp.Channels[0])
	}
}

func TestNewlyAddedModels(t *testing.T) {
	added := newlyAddedModels([]string{"gpt-5.5", "GPT-5.4"}, []string{"gpt-5.4", "gpt-6-astra", "gpt-5.5", "gpt-6-astra", " "})
	if len(added) != 1 || added[0] != "gpt-6-astra" {
		t.Fatalf("added = %v", added)
	}
}
