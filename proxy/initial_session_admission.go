package proxy

import (
	"encoding/binary"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/codex2api/api"
	"github.com/codex2api/database"
	"github.com/google/uuid"
)

// 首次会话准入：Codex 原生客户端、亲和键还没有账号绑定的会话，第一次选号前
// 用会话 ID(UUIDv7)内置的毫秒时间戳算年龄。太旧的 ID 在这里出现，要么是
// 客户端在重放别处创建的旧会话去抢绑定，要么是网关自己丢了绑定(TTL/重启)。
// 后者是运营代价(见 UI 说明),前者是本功能要挡的。非 v7 的 ID 只计数不拒绝:
// 威胁模型是重放真实旧 ID,真实 Codex ID 必然是 v7;伪造 v4 只会建一个新绑定。

type initialSessionVerdict string

const (
	initialSessionAllowed initialSessionVerdict = "allowed"
	initialSessionExpired initialSessionVerdict = "expired"
	initialSessionFuture  initialSessionVerdict = "future"
	initialSessionInvalid initialSessionVerdict = "invalid"

	// 客户端时钟略快是常态(尤其 Windows 桌面),30 秒内的"未来"视为合法。
	initialSessionFutureTolerance = 30 * time.Second
)

func evaluateInitialSessionAge(id string, received time.Time, maxAge time.Duration) (initialSessionVerdict, time.Duration) {
	u, err := uuid.Parse(strings.TrimSpace(id))
	if err != nil || u.Version() != 7 || u.Variant() != uuid.RFC4122 {
		return initialSessionInvalid, 0
	}
	var raw [8]byte
	copy(raw[2:], u[:6])
	idTime := time.UnixMilli(int64(binary.BigEndian.Uint64(raw[:])))
	age := received.Sub(idTime)
	switch {
	case age < -initialSessionFutureTolerance:
		return initialSessionFuture, age
	case age > maxAge:
		return initialSessionExpired, age
	default:
		return initialSessionAllowed, age
	}
}

type SessionGuardInitialSummary struct {
	Samples          uint64  `json:"samples"`
	Allowed          uint64  `json:"allowed"`
	Expired          uint64  `json:"expired"`
	Future           uint64  `json:"future"`
	Invalid          uint64  `json:"invalid"`
	MaxAgeMillis     int64   `json:"max_age_ms"`
	AverageAgeMillis float64 `json:"average_age_ms"`
	sumAgeMillis     float64
	validSamples     uint64
}

func (s *SessionGuardInitialSummary) add(verdict initialSessionVerdict, age time.Duration) {
	s.Samples++
	switch verdict {
	case initialSessionAllowed:
		s.Allowed++
	case initialSessionExpired:
		s.Expired++
	case initialSessionFuture:
		s.Future++
	default:
		s.Invalid++
		return
	}
	if verdict == initialSessionFuture {
		return
	}
	ms := age.Milliseconds()
	s.validSamples++
	s.sumAgeMillis += float64(ms)
	if ms > s.MaxAgeMillis {
		s.MaxAgeMillis = ms
	}
}

func (s *SessionGuardInitialSummary) merge(o SessionGuardInitialSummary) {
	s.Samples += o.Samples
	s.Allowed += o.Allowed
	s.Expired += o.Expired
	s.Future += o.Future
	s.Invalid += o.Invalid
	s.validSamples += o.validSamples
	s.sumAgeMillis += o.sumAgeMillis
	if o.MaxAgeMillis > s.MaxAgeMillis {
		s.MaxAgeMillis = o.MaxAgeMillis
	}
}

func (s *SessionGuardInitialSummary) finish() {
	if s.validSamples > 0 {
		s.AverageAgeMillis = s.sumAgeMillis / float64(s.validSamples)
	}
}

type initialSessionBucket struct {
	second  int64
	summary SessionGuardInitialSummary
}

type initialSessionStatsState struct {
	mu      sync.Mutex
	total   SessionGuardInitialSummary
	buckets [3600]initialSessionBucket
}

var initialSessionStats = &initialSessionStatsState{}

func resetInitialSessionStatsForTest() {
	initialSessionStats.mu.Lock()
	initialSessionStats.total = SessionGuardInitialSummary{}
	initialSessionStats.buckets = [3600]initialSessionBucket{}
	initialSessionStats.mu.Unlock()
}

func recordInitialSessionVerdict(received time.Time, verdict initialSessionVerdict, age time.Duration) {
	s := initialSessionStats
	s.mu.Lock()
	defer s.mu.Unlock()
	s.total.add(verdict, age)
	second := received.Unix()
	b := &s.buckets[((second%3600)+3600)%3600]
	if b.second > second {
		return
	}
	if b.second != second {
		*b = initialSessionBucket{second: second}
	}
	b.summary.add(verdict, age)
}

func sessionGuardInitialSnapshot(now time.Time) (recentHour, sinceStart SessionGuardInitialSummary) {
	s := initialSessionStats
	s.mu.Lock()
	defer s.mu.Unlock()
	sinceStart = s.total
	floor := now.Unix() - 3600
	for _, b := range s.buckets {
		if b.second <= floor || b.second > now.Unix() {
			continue
		}
		recentHour.merge(b.summary)
	}
	recentHour.finish()
	sinceStart.finish()
	return recentHour, sinceStart
}

func isCodexNativeRequest(headers http.Header, body []byte) bool {
	if headers == nil {
		return false
	}
	if EvaluateEngineFingerprint(headers, body, nil) {
		return true
	}
	return IsCodexOfficialClientByHeaders(headers.Get("User-Agent"), headers.Get("Originator"))
}

func initialSessionAdmissionError() *api.APIError {
	return api.NewAPIErrorWithDetails(
		api.ErrorCode("codex_session_identity_unavailable"),
		"当前会话无法继续处理,请重新打开对话;仍失败时请新建对话。",
		api.ErrorTypeInvalidRequest,
		// gin.H 是具名类型，err.Details.(map[string]any) 断言不到它的底层类型，
		// 必须用字面 map[string]any 让调用方（及测试）能直接断言取出 retry 字段。
		map[string]any{"retry": "stop"},
	)
}

// checkInitialSessionAdmission 在 affinityKey / turnHasBinding 算出之后、第一次选号
// 之前调用。返回非 nil 即拒绝。hasBinding=true 或非 Codex 原生请求直接放行。
func (h *Handler) checkInitialSessionAdmission(headers http.Header, body []byte, identity requestSessionIdentity, hasBinding bool, received time.Time) *api.APIError {
	settings := CurrentRuntimeSettings()
	if !settings.CodexInitialSessionAdmissionEnabled || hasBinding {
		return nil
	}
	sessionID := strings.TrimSpace(identity.explicitUpstreamID)
	if sessionID == "" || !isCodexNativeRequest(headers, body) {
		return nil
	}
	maxAge := time.Duration(database.NormalizeCodexInitialSessionMaxAgeSeconds(settings.CodexInitialSessionMaxAgeSeconds)) * time.Second
	verdict, age := evaluateInitialSessionAge(sessionID, received, maxAge)
	recordInitialSessionVerdict(received, verdict, age)
	if verdict == initialSessionExpired || verdict == initialSessionFuture {
		log.Printf("[INITIAL-SESSION] rejected verdict=%s age=%s limit=%s session=%s", verdict, age.Round(time.Millisecond), maxAge, hashRiskIdentity(sessionID))
		return initialSessionAdmissionError()
	}
	return nil
}
