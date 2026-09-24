package proxy

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
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

// Keep the runtime response schema compatible with existing admin clients.
// The sever branch no longer samples or enforces first-session ID admission.
type InitialSessionAgeSummary struct {
	Samples       uint64  `json:"samples"`
	ValidSamples  uint64  `json:"valid_samples"`
	Allowed       uint64  `json:"allowed"`
	Rejected      uint64  `json:"rejected"`
	Invalid       uint64  `json:"invalid"`
	Future        uint64  `json:"future"`
	AverageMillis float64 `json:"average_ms"`
	MaxMillis     int64   `json:"max_ms"`
}

type InitialSessionAgeStatus struct {
	Enabled      bool                     `json:"enabled"`
	StartedAt    time.Time                `json:"started_at"`
	LimitSeconds int                      `json:"limit_seconds"`
	RecentHour   InitialSessionAgeSummary `json:"recent_hour"`
	SinceStart   InitialSessionAgeSummary `json:"since_start"`
}

var initialSessionStartedAt = time.Now().UTC()

func GetInitialSessionAgeStatus() InitialSessionAgeStatus {
	return InitialSessionAgeStatus{
		Enabled:      false,
		StartedAt:    initialSessionStartedAt,
		LimitSeconds: database.NormalizeCodexInitialSessionMaxAgeSeconds(CurrentRuntimeSettings().CodexInitialSessionMaxAgeSeconds),
	}
}

// A first account binding still needs stale turn-state cleanup. It does not
// require a UUIDv7 or a client timestamp, regardless of legacy age settings.
func prepareInitialSession(request *gin.Context) {
	state := usageRequestDiagnosticState(request)
	if state.InitialSession == nil {
		state.InitialSession = &initialSessionDiagnostic{
			Result:           "disabled",
			ReceivedAt:       state.StartedAt,
			AgeCheckDisabled: true,
			IdentitySource:   "original_root_session_id",
		}
	}
	request.Request = request.Request.WithContext(context.WithValue(request.Request.Context(), initialSessionContextKey{}, state.InitialSession))
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
