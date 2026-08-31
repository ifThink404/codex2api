package admin

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/codex2api/cache"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
)

func TestCollectInviteEmails(t *testing.T) {
	t.Run("dedup and trim from list + text", func(t *testing.T) {
		got, err := collectInviteEmails(
			[]string{"A@example.com", " b@example.com "},
			"a@example.com\nc@example.com, d@example.com",
			10,
		)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		// A@ 与 a@ 视为重复（忽略大小写），保留首次出现的大小写形式。
		want := []string{"A@example.com", "b@example.com", "c@example.com", "d@example.com"}
		if len(got) != len(want) {
			t.Fatalf("got %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("got[%d]=%q, want %q (full: %v)", i, got[i], want[i], got)
			}
		}
	})

	t.Run("rejects invalid email", func(t *testing.T) {
		if _, err := collectInviteEmails([]string{"not-an-email"}, "", 10); err == nil {
			t.Fatal("expected error for invalid email")
		}
	})

	t.Run("empty input errors", func(t *testing.T) {
		if _, err := collectInviteEmails(nil, "  ", 10); err == nil {
			t.Fatal("expected error for empty input")
		}
	})

	t.Run("exceeds cap", func(t *testing.T) {
		if _, err := collectInviteEmails([]string{"a@x.com", "b@x.com", "c@x.com"}, "", 2); err == nil {
			t.Fatal("expected error when exceeding cap")
		}
	})
}

func newInviteCacheTestHandler(t *testing.T, withCache bool) *Handler {
	t.Helper()
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "invite-cache.db"))
	if err != nil {
		t.Fatalf("database.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	h := &Handler{db: db}
	if withCache {
		h.cache = cache.NewMemory(1)
		t.Cleanup(func() { _ = h.cache.Close() })
	}
	return h
}

type inviteCacheProbe struct {
	Value string `json:"value"`
}

func TestInviteCacheReadWrite(t *testing.T) {
	ctx := context.Background()
	const scope = "prog|persistent"

	t.Run("snapshot path serves after restart-equivalent cold cache", func(t *testing.T) {
		// cache=nil 等价于「进程重启、运行态缓存全空」：数据必须还能从库里读回来。
		h := newInviteCacheTestHandler(t, false)
		h.writeInviteCache(ctx, inviteEligibilityCacheNamespace, database.CodexInviteSnapshotEligibility,
			42, 3, scope, 200, time.Minute, inviteCacheProbe{Value: "from-upstream"})

		var got inviteCacheProbe
		meta := h.readInviteCache(ctx, inviteEligibilityCacheNamespace,
			database.CodexInviteSnapshotEligibility, 42, 3, scope, &got)
		if meta == nil {
			t.Fatal("expected snapshot hit, got miss")
		}
		if meta.Source != "snapshot" {
			t.Fatalf("expected source=snapshot, got %q", meta.Source)
		}
		if got.Value != "from-upstream" {
			t.Fatalf("payload mismatch: %+v", got)
		}
	})

	t.Run("runtime cache short-circuits the database", func(t *testing.T) {
		h := newInviteCacheTestHandler(t, true)
		h.writeInviteCache(ctx, inviteEligibilityCacheNamespace, database.CodexInviteSnapshotEligibility,
			42, 3, scope, 200, time.Minute, inviteCacheProbe{Value: "hot"})

		var got inviteCacheProbe
		meta := h.readInviteCache(ctx, inviteEligibilityCacheNamespace,
			database.CodexInviteSnapshotEligibility, 42, 3, scope, &got)
		if meta == nil || meta.Source != "runtime" {
			t.Fatalf("expected source=runtime, got %+v", meta)
		}
		if got.Value != "hot" {
			t.Fatalf("payload mismatch: %+v", got)
		}
	})

	t.Run("credential generation change misses", func(t *testing.T) {
		// 重新授权后资格属于另一份身份，旧快照不能继续端出来。
		h := newInviteCacheTestHandler(t, true)
		h.writeInviteCache(ctx, inviteEligibilityCacheNamespace, database.CodexInviteSnapshotEligibility,
			42, 3, scope, 200, time.Minute, inviteCacheProbe{Value: "old-identity"})

		var got inviteCacheProbe
		if meta := h.readInviteCache(ctx, inviteEligibilityCacheNamespace,
			database.CodexInviteSnapshotEligibility, 42, 4, scope, &got); meta != nil {
			t.Fatalf("expected miss after generation bump, got %+v (%+v)", meta, got)
		}
	})

	t.Run("expired snapshot misses", func(t *testing.T) {
		h := newInviteCacheTestHandler(t, false)
		h.writeInviteCache(ctx, inviteTrackingCacheNamespace, database.CodexInviteSnapshotTracking,
			42, 1, scope, 200, -time.Minute, inviteCacheProbe{Value: "stale"})

		var got inviteCacheProbe
		if meta := h.readInviteCache(ctx, inviteTrackingCacheNamespace,
			database.CodexInviteSnapshotTracking, 42, 1, scope, &got); meta != nil {
			t.Fatalf("expected miss for expired snapshot, got %+v", meta)
		}
	})

	t.Run("invalidate clears both layers", func(t *testing.T) {
		h := newInviteCacheTestHandler(t, true)
		programID, entrypoint := proxy.NormalizeInviteProgram("", "")
		eligScope := inviteEligibilityScope(programID, entrypoint)
		h.writeInviteCache(ctx, inviteEligibilityCacheNamespace, database.CodexInviteSnapshotEligibility,
			42, 1, eligScope, 200, time.Minute, inviteCacheProbe{Value: "pre-send"})

		h.invalidateInviteCache(ctx, 42, 1, programID, entrypoint)

		var got inviteCacheProbe
		if meta := h.readInviteCache(ctx, inviteEligibilityCacheNamespace,
			database.CodexInviteSnapshotEligibility, 42, 1, eligScope, &got); meta != nil {
			t.Fatalf("expected miss after invalidation, got %+v (%+v)", meta, got)
		}
	})
}

// 归一化后的参数才进作用域，否则「不传」与「传默认值」会各存一份相同内容。
func TestInviteCacheScopeNormalization(t *testing.T) {
	programA, entryA := proxy.NormalizeInviteProgram("", "")
	programB, entryB := proxy.NormalizeInviteProgram(proxy.DefaultProgramID, proxy.DefaultEntrypoint)
	if inviteEligibilityScope(programA, entryA) != inviteEligibilityScope(programB, entryB) {
		t.Fatal("empty and explicit-default program params must share one scope")
	}

	periodA, limitA := proxy.NormalizeInviteTracking("", 0)
	periodB, limitB := proxy.NormalizeInviteTracking(periodA, limitA)
	if inviteTrackingScope(programA, periodA, limitA) != inviteTrackingScope(programB, periodB, limitB) {
		t.Fatal("tracking defaults must normalize to one scope")
	}
	if inviteTrackingScope(programA, periodA, 10) == inviteTrackingScope(programA, periodA, limitA) {
		t.Fatal("different limits must not share a scope")
	}
}
