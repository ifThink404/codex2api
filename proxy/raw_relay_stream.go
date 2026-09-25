package proxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

type rawRelayIOError struct {
	Kind    string `json:"kind"`
	Type    string `json:"type"`
	Message string `json:"message"`
}

func rawRelayErrorDetail(c *gin.Context, err error) *rawRelayIOError {
	if err == nil || errors.Is(err, io.EOF) {
		return nil
	}
	d := &rawRelayIOError{Kind: "io_error", Type: fmt.Sprintf("%T", err), Message: upstreamErrorSafeMessage(c, err.Error())}
	var networkError net.Error
	switch {
	case errors.Is(err, context.Canceled):
		d.Kind = "context_canceled"
	case errors.Is(err, context.DeadlineExceeded):
		d.Kind = "deadline_exceeded"
	case errors.Is(err, io.ErrUnexpectedEOF):
		d.Kind = "unexpected_eof"
	case errors.Is(err, io.ErrShortWrite):
		d.Kind = "short_write"
	case errors.Is(err, net.ErrClosed), errors.Is(err, io.ErrClosedPipe):
		d.Kind = "connection_closed"
	case errors.As(err, &networkError) && networkError.Timeout():
		d.Kind = "timeout"
	}
	return d
}

// HTTP status and raw bytes are independent of this diagnostic. A completed
// protocol event can precede a transport error or the client's cancellation;
// retain both facts without turning successful generation into a read failure.
func (d *rawRelayDiagnostic) finishStream(c *gin.Context, o *rawRelayUsageObserver, readErr, writeErr, ctxErr error) {
	d.ReadError, d.WriteError = rawRelayErrorDetail(c, readErr), rawRelayErrorDetail(c, writeErr)
	d.ClientCanceled = errors.Is(ctxErr, context.Canceled)
	if ctxErr != nil {
		d.RequestContextError = upstreamErrorSafeMessage(c, ctxErr.Error())
	}
	switch {
	case writeErr != nil:
		d.StreamEnd = "write_error"
	case errors.Is(readErr, io.EOF):
		d.StreamEnd = "eof"
	case readErr != nil:
		d.StreamEnd = "read_error"
	}
	if o.stream {
		d.TerminalEvent, d.ResponseStatus, d.IncompleteReason = o.terminalEvent, o.responseStatus, o.incompleteReason
		d.StreamOutcome = "unknown"
		if o.terminalEvent != "" {
			switch o.responseStatus {
			case "completed", "failed", "incomplete":
				d.StreamOutcome = o.responseStatus
			case "canceled", "cancelled":
				d.StreamOutcome = "incomplete"
			}
		}
		if d.StreamOutcome == "failed" || d.StreamOutcome == "incomplete" {
			d.TerminalError = upstreamErrorSafeMessage(c, o.errorMessage)
			d.ResponseError = "upstream_response_" + d.StreamOutcome
			return
		}
	}
	switch {
	case writeErr != nil:
		d.ResponseError = "downstream_write_failed"
	case d.StreamOutcome == "completed":
		// Only a complete SSE frame accepted by Write qualifies; usage alone
		// and a partial terminal line never suppress a transport failure.
		return
	case d.ReadError != nil && errors.Is(ctxErr, context.Canceled):
		d.ResponseError = "downstream_canceled"
	case d.ReadError != nil && errors.Is(ctxErr, context.DeadlineExceeded):
		d.ResponseError = "request_deadline_exceeded"
	case d.ReadError != nil:
		d.ResponseError = "upstream_read_failed"
	}
	if d.ResponseError != "" && o.stream {
		d.StreamOutcome = "interrupted"
	}
}

// Assemble bounded SSE events for observation only. The forwarding path never
// waits for this parser and never reconstructs the event's bytes.
func (o *rawRelayUsageObserver) observeSSELine(line []byte) {
	if len(line) == 0 {
		if !o.discardEvent && len(o.eventData) > 0 {
			if gjson.ValidBytes(o.eventData) {
				o.observe(o.eventData)
			}
			o.observeSSETerminal(o.eventName, bytes.TrimSpace(o.eventData))
		}
		o.eventName, o.eventData, o.discardEvent = "", nil, false
		return
	}
	field, value, _ := bytes.Cut(line, []byte{':'})
	value = bytes.TrimPrefix(value, []byte{' '})
	switch string(field) {
	case "event":
		if len(value) <= 128 {
			o.eventName = string(value)
		} else {
			o.eventName = ""
		}
	case "data":
		if o.discardEvent {
			return
		}
		if len(o.eventData)+len(value)+1 > 4<<20 {
			o.eventData, o.discardEvent = nil, true
			return
		}
		o.eventData = append(o.eventData, value...)
		o.eventData = append(o.eventData, '\n')
	}
}

func (o *rawRelayUsageObserver) observeSSETerminal(event string, data []byte) {
	if bytes.Equal(data, []byte("[DONE]")) {
		// Chat's sentinel cannot overwrite a Responses failure or stand in
		// for a missing Responses terminal event.
		if o.terminalEvent == "" && !o.sawResponses {
			o.terminalEvent, o.responseStatus = "[DONE]", "completed"
		}
		return
	}
	if !gjson.ValidBytes(data) {
		return
	}
	root := gjson.ParseBytes(data)
	if !root.IsObject() {
		return
	}
	if typ := root.Get("type"); typ.Type == gjson.String && typ.String() != "" {
		event = typ.String()
	}
	if strings.HasPrefix(event, "response.") {
		o.sawResponses = true
	}
	if (event == "" || event == "message") && root.Get("error").IsObject() {
		event = "error"
	}
	status := root.Get("response.status").String()
	switch event {
	case "response.completed", "message_stop":
		if status == "" {
			status = "completed"
		}
	case "response.failed", "response.error", "error":
		status = "failed"
	case "response.incomplete":
		status = "incomplete"
	case "response.canceled", "response.cancelled":
		status = "canceled"
	case "response.done":
		// Legacy terminal event: success requires an explicit status.
	default:
		return
	}
	switch status {
	case "completed", "failed", "incomplete", "canceled", "cancelled":
	default:
		status = "unknown"
	}
	// Once the upstream reported failure, a later generic stop or sentinel
	// must not relabel it as successful completion.
	if o.responseStatus == "failed" || o.responseStatus == "incomplete" || o.responseStatus == "canceled" || o.responseStatus == "cancelled" {
		return
	}
	o.terminalEvent, o.responseStatus = event, status
	if status == "failed" && o.errorMessage == "" {
		if message := root.Get("message"); message.Type == gjson.String {
			o.errorMessage = message.String()
			if len(o.errorMessage) > 4096 {
				o.errorMessage = o.errorMessage[:4096]
			}
		}
	}
	switch reason := root.Get("response.incomplete_details.reason").String(); reason {
	case "", "max_output_tokens", "content_filter":
		o.incompleteReason = reason
	default:
		o.incompleteReason = "other"
	}
}
