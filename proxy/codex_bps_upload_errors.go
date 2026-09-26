package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"

	"github.com/codex2api/api"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

type bpsUploadFailureDiagnostic struct {
	Stage      string `json:"stage"`
	HTTPStatus int    `json:"http_status,omitempty"`
	Code       string `json:"code,omitempty"`
	Type       string `json:"type,omitempty"`
	Message    string `json:"message,omitempty"`
	RequestID  string `json:"request_id,omitempty"`
	CFRay      string `json:"cf_ray,omitempty"`
}

var bpsDiagnosticFileID = regexp.MustCompile(`\bfile-[A-Za-z0-9_-]+`)

// The attachment body, filename, credentials and file handles are never logged.
// Keep only the bounded error fields needed to distinguish network failures,
// rate limits, policy rejections and an unexpected success response shape.
func bpsUploadSafeText(value, filename string, headers http.Header) string {
	for _, secret := range []string{filename, strings.TrimPrefix(headers.Get("Authorization"), "Bearer "), headers.Get("Cookie"), headers.Get("ChatGPT-Account-Id")} {
		if secret != "" {
			value = strings.ReplaceAll(value, secret, "[REDACTED]")
		}
	}
	value = bpsDiagnosticFileID.ReplaceAllString(value, "[file]")
	return upstreamErrorSafeMessage(nil, value)
}

type bpsAttachmentUploadError struct {
	failure *Error
	detail  bpsUploadFailureDiagnostic
}

func (e *bpsAttachmentUploadError) Error() string           { return e.failure.Error() }
func (e *bpsAttachmentUploadError) Unwrap() error           { return e.failure }
func (e *bpsAttachmentUploadError) UpstreamStatusCode() int { return e.failure.HTTPStatus }
func (e *bpsAttachmentUploadError) UpstreamErrorBody() []byte {
	code, kind := e.detail.Code, e.detail.Type
	if code == "" {
		code = e.failure.Code
	}
	if kind == "" {
		kind = e.failure.Type
	}
	body, _ := json.Marshal(gin.H{"error": gin.H{"message": e.detail.Message, "code": code, "type": kind}})
	return body
}

// Uploads fail before an inference response exists, but their HTTP 429 still
// belongs to the independent rate-limit budget. Other request errors retain
// the general budget and the existing replay, policy and cancellation checks.
func requestErrorRetryBudget(err error, generalRetries, rateLimitRetries *int, generalLimit, rateLimit int) (*int, int) {
	var upload *bpsAttachmentUploadError
	if errors.As(err, &upload) && upload.UpstreamStatusCode() == http.StatusTooManyRequests {
		return rateLimitRetries, rateLimit
	}
	return generalRetries, generalLimit
}

// Preparation failures never reach the inference response logging branches.
// Record exactly one zero-token attempt while its account/transport trace is
// still current, including failures that will be retried on another account.
func (h *Handler) logBPSPreparationFailure(c *gin.Context, err error, input *database.UsageLogInput, serviceTier string) {
	var upload *bpsAttachmentUploadError
	if !errors.As(err, &upload) {
		return
	}
	body := upload.UpstreamErrorBody()
	captureUpstreamErrorDiagnostic(c, body, upload.detail.HTTPStatus, "upstream_http", "bps_attachment_upload")
	input.StatusCode = upload.UpstreamStatusCode()
	input.UpstreamErrorKind = "bps_attachment_upload"
	input.ErrorMessage = usageLogErrorMessage(input.StatusCode, body)
	input.InboundEndpoint = input.Endpoint
	usageTiers := resolveUsageServiceTiers("", serviceTier)
	input.ServiceTier = usageTiers.ServiceTier
	input.RequestedServiceTier = usageTiers.RequestedServiceTier
	input.ActualServiceTier = usageTiers.ActualServiceTier
	input.BillingServiceTier = usageTiers.BillingServiceTier
	h.logUsageForRequest(c, input)
}

func bpsAttachmentFailure(ctx context.Context, headers http.Header, filename, stage string, status int, responseHeaders http.Header, raw []byte, cause error) error {
	d := bpsUploadFailureDiagnostic{Stage: stage, HTTPStatus: status,
		RequestID: safeDiagnosticToken(responseHeaders.Get("X-Request-Id")), CFRay: safeDiagnosticToken(responseHeaders.Get("CF-Ray"))}
	for _, path := range []string{"error", "detail.error.error", "detail.error", "detail", ""} {
		value := gjson.ParseBytes(raw)
		if path != "" {
			value = value.Get(path)
		}
		// Unstructured top-level messages may be arbitrary file contents. Only
		// retain a recognized error envelope or an explicitly typed root error.
		if path == "" && value.Get("code").String() == "" && value.Get("type").String() == "" {
			continue
		}
		if code := value.Get("code").String(); code != "" {
			d.Code = code
		}
		if kind := value.Get("type").String(); kind != "" {
			d.Type = kind
		}
		if message := value.Get("message"); message.Type == gjson.String {
			d.Message = message.String()
			break
		}
		if value.Type == gjson.String {
			d.Message = value.String()
			break
		}
	}
	if d.Message == "" && cause != nil {
		d.Message = cause.Error()
	}
	if d.Message == "" {
		if stage == "response_shape" {
			d.Message = "Attachment upload response has no valid openai_file_id"
		} else {
			d.Message = "Attachment endpoint returned an error without a JSON error message"
		}
	}
	d.Message = bpsUploadSafeText(d.Message, filename, headers)
	d.Code = safeDiagnosticToken(bpsUploadSafeText(d.Code, filename, headers))
	d.Type = safeDiagnosticToken(bpsUploadSafeText(d.Type, filename, headers))
	bpsTimingFromContext(ctx).uploadFailed(d)
	if stage == "canceled" {
		return cause
	}
	clientStatus := status
	if clientStatus < 400 || clientStatus > 599 {
		clientStatus = http.StatusBadGateway
	}
	return &bpsAttachmentUploadError{failure: ErrUpstream(clientStatus, "附件上传失败。", errors.New(d.Message)), detail: d}
}

func bpsPreparationFailureForRequest(c *gin.Context) *bpsAttachmentUploadError {
	if c == nil || c.Request == nil {
		return nil
	}
	transport := snapshotUpstreamTrace(c.Request.Context()).Transport
	if transport == nil || !strings.HasPrefix(transport.ErrorStage, "bps_") || transport.BPS == nil || transport.BPS.Timing == nil {
		return nil
	}
	d := transport.BPS.Timing.uploadFailure()
	if d == nil || d.Stage == "canceled" {
		return nil
	}
	status := d.HTTPStatus
	if status < 400 || status > 599 {
		status = http.StatusBadGateway
	}
	return &bpsAttachmentUploadError{failure: ErrUpstream(status, "附件上传失败。", errors.New(d.Message)), detail: *d}
}

func (handler *Handler) sendBPSPreparationFailure(c *gin.Context, stream, chat bool) bool {
	if sessionModelErrorForRequest(c) != nil {
		return false
	}
	if plan, _ := c.Request.Context().Value(sessionAccountFailoverContextKey{}).(*sessionAccountFailoverPlan); plan != nil && plan.Failure != nil {
		return false
	}
	failure := bpsPreparationFailureForRequest(c)
	if failure == nil {
		return false
	}
	captureUpstreamErrorDiagnostic(c, failure.UpstreamErrorBody(), failure.detail.HTTPStatus, "upstream_http", "bps_attachment_upload")
	handler.recordServiceError(c, failure.UpstreamStatusCode(), api.NewAPIError(
		api.ErrorCode("codex_dispatch_bps_upload_retry_unavailable"),
		"BPS 附件上传失败后没有可用的重试账号。", api.ErrorTypeServer))
	if stream && c.Writer.Written() {
		if chat {
			return writeCommittedChatRetryError(c, failure.detail.Message)
		}
		return writeCommittedResponsesRetryError(c, failure.detail.Message)
	}
	ErrorToGinResponse(c, failure)
	return true
}
