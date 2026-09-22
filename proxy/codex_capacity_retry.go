package proxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/codex2api/api"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const codexCapacityResponsesStreamKey = "codex_capacity_responses_stream"
const codexOverloadRetryDefault = 30 * time.Second

var codexCapacityRetryAdvice = regexp.MustCompile(`(?i)try again in\s*(\d+(?:\.\d+)?)\s*(ms|seconds?|s)`)

func codexModelCapacityTerminalMessage(message string) bool {
	message = strings.ToLower(message)
	return strings.Contains(message, "selected model is at capacity") ||
		strings.Contains(message, "model is at capacity. please try a different model") ||
		strings.Contains(message, "model is disabled") || strings.Contains(message, "model has been disabled")
}

// Only the public copy uses rate_limit_exceeded: older Codex versions treat
// both server_is_overloaded and slow_down as terminal. The rate-limit event
// carries retry advice on both old and new versions.
// The upstream payload and its accounting/capacity classification stay intact.
func codexCapacityRetryableClientError(err *api.APIError) bool {
	return err != nil && err.Code == "rate_limit_exceeded" && strings.HasPrefix(err.Message, "The upstream service is temporarily overloaded.")
}

func codexCapacityRetryDelay(message string) time.Duration {
	parts := codexCapacityRetryAdvice.FindStringSubmatch(message)
	if len(parts) == 3 {
		value, err := strconv.ParseFloat(parts[1], 64)
		if strings.EqualFold(parts[2], "ms") {
			value /= 1000
		}
		if err == nil && value > 0 && value <= maxRetryAfterDelay.Seconds() {
			return time.Duration(value * float64(time.Second))
		}
	}
	return codexOverloadRetryDefault
}

func codexCapacityRetryMessage(delay time.Duration) string {
	return fmt.Sprintf("The upstream service is temporarily overloaded. Please try again in %ss.", strconv.FormatFloat(delay.Seconds(), 'f', -1, 64))
}

func codexCapacityFailedEvent(err *api.APIError) gin.H {
	return gin.H{"type": "response.failed", "response": gin.H{
		"created_at": time.Now().Unix(), "status": "failed", "error": err,
	}}
}

func isCodexCapacityCodeOrMessage(code, message string) bool {
	switch strings.ToLower(strings.TrimSpace(code)) {
	case "server_is_overloaded", "slow_down":
		return true
	}
	message = strings.ToLower(message)
	return strings.Contains(message, "selected model is at capacity") ||
		strings.Contains(message, "model is at capacity. please try a different model")
}

func codexCapacityErrorForClient(body []byte) *api.APIError {
	if !gjson.ValidBytes(body) {
		return nil
	}
	parsed := gjson.ParseBytes(body)
	for _, path := range []string{"error", "response.error", "response.status_details.error", "detail", ""} {
		object := parsed
		if path != "" {
			object = object.Get(path)
		}
		code := strings.ToLower(strings.TrimSpace(object.Get("code").String()))
		message := strings.TrimSpace(object.Get("message").String())
		if object.Type == gjson.String {
			message = strings.TrimSpace(object.String())
		}
		if !isCodexCapacityCodeOrMessage(code, message) {
			continue
		}
		if code == "slow_down" || (code == "server_is_overloaded" && !codexModelCapacityTerminalMessage(message)) {
			return api.NewAPIError("rate_limit_exceeded", codexCapacityRetryMessage(codexCapacityRetryDelay(message)), "rate_limit_error")
		}
		if code != "slow_down" {
			code = "server_is_overloaded"
		}
		return api.NewAPIError(api.ErrorCode(code), publicUpstreamMessage(code), "service_unavailable_error")
	}
	return nil
}

func codexCapacityRequestError(err error) *api.APIError {
	var upstreamError continuousRetryHTTPError
	if errors.As(err, &upstreamError) {
		return codexCapacityErrorForClient(upstreamError.UpstreamErrorBody())
	}
	return nil
}

func codexCapacityRetryDisabled(body []byte) bool {
	return !CurrentRuntimeSettings().CodexCapacityRetryEnabled && codexCapacityErrorForClient(body) != nil
}

func codexCapacityRequestRetryDisabled(err error) bool {
	return !CurrentRuntimeSettings().CodexCapacityRetryEnabled && codexCapacityRequestError(err) != nil
}

func writeCodexCapacityError(c *gin.Context, body []byte, protocol continuousRetryHTTPProtocol, upstreamStatus ...int) bool {
	capacityError := codexCapacityErrorForClient(body)
	if capacityError == nil {
		return false
	}
	retryable := codexCapacityRetryableClientError(capacityError)
	status := http.StatusBadRequest
	if retryable {
		status = http.StatusServiceUnavailable
		if len(upstreamStatus) > 0 && (upstreamStatus[0] == 429 || upstreamStatus[0] >= 500 && upstreamStatus[0] <= 599) {
			status = upstreamStatus[0]
		}
		delay := codexCapacityRetryDelay(capacityError.Message)
		if header := normalizedRetryAfter(c.Writer.Header().Get("Retry-After")); header != "" {
			if suggested := parseRetryAfterHeader(header); suggested > 0 && suggested <= maxRetryAfterDelay {
				delay = suggested
			}
		}
		capacityError.Message = codexCapacityRetryMessage(delay)
		// Retry-After uses whole seconds; the event message retains subsecond advice.
		c.Header("Retry-After", strconv.FormatInt(int64((delay+time.Second-1)/time.Second), 10))
	} else {
		c.Writer.Header().Del("Retry-After")
	}
	var envelope any = api.ErrorResponse{Error: *capacityError}
	if protocol == continuousRetryProtocolAnthropic {
		envelope = gin.H{"type": "error", "error": capacityError}
	}
	responsesStream := retryable && c.GetBool(codexCapacityResponsesStreamKey)
	if !c.Writer.Written() && !responsesStream {
		c.Header("Content-Type", "application/json; charset=utf-8")
		c.JSON(status, envelope)
		return true
	}
	if responsesStream && !c.Writer.Written() {
		c.Header("Content-Type", "text/event-stream")
		c.Header("Cache-Control", "no-cache")
		c.Status(http.StatusOK)
	}
	if protocol == continuousRetryProtocolResponses || responsesStream {
		envelope = codexCapacityFailedEvent(capacityError)
	}
	payload, _ := json.Marshal(envelope)
	prefix := "data: "
	if protocol == continuousRetryProtocolAnthropic {
		prefix = "event: error\ndata: "
	}
	_, _ = c.Writer.WriteString(prefix + string(payload) + "\n\n")
	c.Writer.Flush()
	return true
}

func writeCommittedCodexCapacityError(c *gin.Context, protocol continuousRetryHTTPProtocol) bool {
	if c.Request == nil {
		return false
	}
	failure, exists := continuousRetryLastFailure(c.Request.Context())
	return exists && writeCodexCapacityError(c, failure.body, protocol, failure.status)
}

func writeResponseFailedHTTPError(c *gin.Context, status int, body []byte, message string) {
	if writeUpstreamPromptSafetyError(c, body) {
		return
	}
	if writeCodexCapacityError(c, body, continuousRetryProtocolOpenAI, status) {
		return
	}
	c.JSON(status, gin.H{"error": publicUpstreamAPIError(c, body, status, "")})
}
