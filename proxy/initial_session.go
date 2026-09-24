package proxy

import (
	"context"
	"encoding/binary"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/codex2api/api"
	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

type initialSessionContextKey struct{}

type initialSessionDiagnostic struct {
	Result             string     `json:"result"`
	ReceivedAt         time.Time  `json:"received_at"`
	IDTime             *time.Time `json:"id_time,omitempty"`
	AgeMillis          int64      `json:"age_ms"`
	LimitSeconds       int        `json:"limit_seconds"`
	AgeCheckDisabled   bool       `json:"age_check_disabled"`
	IdentitySource     string     `json:"identity_source"`
	HeaderStateRemoved bool       `json:"header_turn_state_removed"`
	BodyStateRemoved   bool       `json:"body_turn_state_removed"`
}

type InitialSessionAgeSummary struct {
	Samples       uint64  `json:"samples"`
	ValidSamples  uint64  `json:"valid_samples"`
	Allowed       uint64  `json:"allowed"`
	Rejected      uint64  `json:"rejected"`
	Invalid       uint64  `json:"invalid"`
	Future        uint64  `json:"future"`
	AverageMillis float64 `json:"average_ms"`
	MaxMillis     int64   `json:"max_ms"`
	sumMillis     float64
}

func (s *InitialSessionAgeSummary) add(d initialSessionDiagnostic) {
	s.Samples++
	switch d.Result {
	case "allowed":
		s.Allowed++
	case "expired":
		s.Rejected++
	case "future":
		s.Future++
	default:
		s.Invalid++
	}
	if d.Result == "allowed" || d.Result == "expired" || d.Result == "future" {
		// Keep signed age in request diagnostics, but aggregate the magnitude so
		// clocks ahead of and behind the gateway do not cancel each other out.
		deviation := d.AgeMillis
		if deviation < 0 {
			deviation = -deviation
		}
		s.ValidSamples++
		s.sumMillis += float64(deviation)
		s.MaxMillis = max(s.MaxMillis, deviation)
	}
}

type initialAgeBucket struct {
	second  int64
	summary InitialSessionAgeSummary
}
type initialAgeStats struct {
	mu      sync.Mutex
	started time.Time
	total   InitialSessionAgeSummary
	buckets [3600]initialAgeBucket
}

var initialSessionStats = initialAgeStats{started: time.Now().UTC()}

func (s *initialAgeStats) record(d initialSessionDiagnostic) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.total.add(d)
	second := d.ReceivedAt.Unix()
	b := &s.buckets[(second%3600+3600)%3600]
	if b.second > second {
		return
	}
	if b.second != second {
		*b = initialAgeBucket{second: second}
	}
	b.summary.add(d)
}

type InitialSessionAgeStatus struct {
	Enabled      bool                     `json:"enabled"`
	StartedAt    time.Time                `json:"started_at"`
	LimitSeconds int                      `json:"limit_seconds"`
	RecentHour   InitialSessionAgeSummary `json:"recent_hour"`
	SinceStart   InitialSessionAgeSummary `json:"since_start"`
}

func (s *initialAgeStats) snapshot(now time.Time, limit int) InitialSessionAgeStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := InitialSessionAgeStatus{StartedAt: s.started, LimitSeconds: limit, SinceStart: s.total}
	for _, b := range s.buckets {
		if b.second <= now.Unix()-3600 || b.second > now.Unix() {
			continue
		}
		r := &result.RecentHour
		r.Samples += b.summary.Samples
		r.ValidSamples += b.summary.ValidSamples
		r.Allowed += b.summary.Allowed
		r.Rejected += b.summary.Rejected
		r.Invalid += b.summary.Invalid
		r.Future += b.summary.Future
		r.sumMillis += b.summary.sumMillis
		r.MaxMillis = max(r.MaxMillis, b.summary.MaxMillis)
	}
	for _, v := range []*InitialSessionAgeSummary{&result.RecentHour, &result.SinceStart} {
		if v.ValidSamples > 0 {
			v.AverageMillis = v.sumMillis / float64(v.ValidSamples)
		}
	}
	return result
}

func GetInitialSessionAgeStatus() InitialSessionAgeStatus {
	settings := CurrentRuntimeSettings()
	status := initialSessionStats.snapshot(time.Now(), database.NormalizeCodexInitialSessionMaxAgeSeconds(settings.CodexInitialSessionMaxAgeSeconds))
	status.Enabled = !settings.CodexInitialSessionAgeCheckDisabled
	return status
}

func evaluateInitialSessionAge(id string, received time.Time, limit int) initialSessionDiagnostic {
	d := initialSessionDiagnostic{Result: "invalid", ReceivedAt: received, LimitSeconds: limit, IdentitySource: "original_root_session_id"}
	u, err := uuid.Parse(strings.TrimSpace(id))
	if err != nil || u.Version() != 7 || u.Variant() != uuid.RFC4122 {
		return d
	}
	var timestamp [8]byte
	copy(timestamp[2:], u[:6])
	idTime := time.UnixMilli(int64(binary.BigEndian.Uint64(timestamp[:]))).UTC()
	d.IDTime = &idTime
	d.AgeMillis = received.UnixMilli() - idTime.UnixMilli()
	// The configured limit applies equally to older IDs and client clocks
	// ahead of the gateway. Both exact boundaries are inclusive.
	limitMillis := int64(limit) * 1000
	switch {
	case d.AgeMillis < -limitMillis:
		d.Result = "future"
	case d.AgeMillis > limitMillis:
		d.Result = "expired"
	default:
		d.Result = "allowed"
	}
	return d
}

func initialSessionAdmissionError() *api.APIError {
	err := api.NewAPIError(api.ErrorCode("codex_session_identity_unavailable"), "当前会话无法继续处理，请重新打开对话；仍失败时请新建对话。", api.ErrorTypeInvalidRequest)
	err.Details = gin.H{"retry": "stop"}
	return err
}

func checkInitialSessionAdmission(request *gin.Context, thread string) *api.APIError {
	state := usageRequestDiagnosticState(request)
	if state.InitialSession == nil {
		settings := CurrentRuntimeSettings()
		d := evaluateInitialSessionAge(thread, state.StartedAt, database.NormalizeCodexInitialSessionMaxAgeSeconds(settings.CodexInitialSessionMaxAgeSeconds))
		d.AgeCheckDisabled = settings.CodexInitialSessionAgeCheckDisabled
		if d.AgeCheckDisabled && d.Result != "invalid" {
			d.Result = "allowed"
		}
		state.InitialSession = &d
		initialSessionStats.record(d)
	}
	if state.InitialSession.Result != "allowed" {
		return initialSessionAdmissionError()
	}
	request.Request = request.Request.WithContext(context.WithValue(request.Request.Context(), initialSessionContextKey{}, state.InitialSession))
	return nil
}

// Run at every executor boundary, before metadata projection and after payload
// rules. A new binding must never inherit a previous account's opaque state.
func PrepareInitialSessionOutbound(ctx context.Context, account *auth.Account, body []byte, headers http.Header) ([]byte, http.Header, error) {
	if ctx == nil || account == nil || account.IsRelayStyle() {
		return body, headers, nil
	}
	d, _ := ctx.Value(initialSessionContextKey{}).(*initialSessionDiagnostic)
	if d == nil {
		return body, headers, nil
	}
	body, headers = rewriteRequestTurnState(body, headers, func(value, carrier string) string {
		if strings.HasPrefix(carrier, "request_header") {
			d.HeaderStateRemoved = true
		} else {
			d.BodyStateRemoved = true
		}
		return ""
	})
	if body == nil {
		return nil, nil, codexAccountIdentityError("当前会话无法继续处理，请新建对话。")
	}
	return body, headers, nil
}
