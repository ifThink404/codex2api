package proxy

import (
	"context"
	"encoding/base64"
	"net/http"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

type usageTurnStateKey struct{}

// Each upstream attempt owns its observation. A late read from an earlier
// attempt cannot overwrite the next attempt, even when accounts are reused.
type usageTurnStateObservation struct {
	mu                   sync.Mutex
	length, decodedBytes *int
}

type usageTurnStateValue struct {
	Source       string `json:"source,omitempty"`
	Length       int    `json:"length"`
	DecodedBytes *int   `json:"decoded_bytes,omitempty"`
}

func measureUsageTurnState(real string) usageTurnStateValue {
	value := usageTurnStateValue{Length: utf8.RuneCountInString(real)}
	if real != "" {
		for _, encoding := range []*base64.Encoding{base64.URLEncoding, base64.RawURLEncoding, base64.StdEncoding, base64.RawStdEncoding} {
			if decoded, err := encoding.Strict().DecodeString(real); err == nil {
				size := len(decoded)
				value.DecodedBytes = &size
				break
			}
		}
	}
	return value
}

// Called with the final HTTP headers/body or current WS frame, after alias
// restoration and account-switch cleanup. Pooled handshake state is excluded.
func captureUsageOutboundTurnState(body []byte, headers http.Header) *usageTurnStateValue {
	var real string
	for name, values := range headers {
		if strings.EqualFold(name, codexTurnStateHeader) && len(values) > 0 {
			real = values[0]
			break
		}
	}
	if real == "" {
		field := gjson.GetBytes(body, "client_metadata.x-codex-turn-state")
		if field.Type == gjson.String {
			real = field.String()
		}
	}
	value := measureUsageTurnState(real)
	return &value
}

func populateUsageOutboundTurnState(input *database.UsageLogInput, upstream *UpstreamTransportDiagnostic) {
	if input == nil || upstream == nil || upstream.AccountID != input.AccountID || upstream.SendPhase != "after_payload" || upstream.RequestTurnState == nil {
		return
	}
	// Prefer a value newly returned by this attempt. Otherwise display the
	// real value actually sent, even when the response does not repeat it.
	if input.TurnStateLength != nil && *input.TurnStateLength > 0 {
		return
	}
	value := upstream.RequestTurnState
	length := value.Length
	input.TurnStateLength = &length
	input.TurnStateDecodedBytes = nil
	if value.DecodedBytes != nil {
		size := *value.DecodedBytes
		input.TurnStateDecodedBytes = &size
	}
}

func beginUsageTurnStateAttempt(c *gin.Context) {
	if c != nil && c.Request != nil {
		c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), usageTurnStateKey{}, &usageTurnStateObservation{}))
	}
}

func observeUsageTurnState(ctx context.Context, real string) {
	observation, _ := ctx.Value(usageTurnStateKey{}).(*usageTurnStateObservation)
	if observation == nil {
		return
	}
	observation.mu.Lock()
	defer observation.mu.Unlock()
	// Keep the first real token for this attempt, matching the header delivered
	// to Codex. An empty HTTP header may be followed by a nonempty WS/SSE event.
	if observation.length != nil && *observation.length > 0 {
		return
	}
	value := measureUsageTurnState(real)
	observation.length, observation.decodedBytes = &value.Length, value.DecodedBytes
	if observer := UpstreamTransportObserver(ctx); observer != nil && observer.attempt.turnState == observation {
		observer.update(func(diagnostic *UpstreamTransportDiagnostic) {
			diagnostic.ResponseTurnState = &value
		})
	}
}

func populateUsageTurnState(c *gin.Context, input *database.UsageLogInput) {
	if c == nil || c.Request == nil || input == nil {
		return
	}
	observation, _ := c.Request.Context().Value(usageTurnStateKey{}).(*usageTurnStateObservation)
	if observation == nil {
		return
	}
	observation.mu.Lock()
	defer observation.mu.Unlock()
	// Copy scalars; the asynchronous log writer must not retain mutable state.
	if observation.length != nil {
		length := *observation.length
		input.TurnStateLength = &length
	}
	if observation.decodedBytes != nil {
		size := *observation.decodedBytes
		input.TurnStateDecodedBytes = &size
	}
}
