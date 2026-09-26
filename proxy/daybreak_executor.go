package proxy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/codex2api/auth"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// 最后一跳在 payload 规则之后固定程序，并再次验证账号与 Key 权限。
func guardDaybreakUpstream(ctx context.Context, account *auth.Account, body []byte) ([]byte, error) {
	guard, _ := ctx.Value(daybreakContextKey{}).(daybreakGuard)
	request := guard.Request
	if request.Program != "" {
		body, _ = sjson.SetBytes(body, "model", request.Model)
		body, _ = sjson.SetBytes(body, "access_programs.cyber", request.Program)
	}
	model := gjson.GetBytes(body, "model").String()
	program := gjson.GetBytes(body, "access_programs.cyber").String()
	if program != auth.DaybreakBlue && program != auth.DaybreakRed {
		return body, nil
	}
	if !account.SupportsDaybreak(model, program) {
		return nil, fmt.Errorf("account does not support requested Daybreak model")
	}
	if msg := checkAPIKeyModel(auth.DaybreakAlias(model, program), guard.Limits); msg != "" {
		return nil, fmt.Errorf("%s", msg)
	}
	log.Printf("[daybreak] account=%d requested=%q model=%q program=%s", account.ID(), request.Requested, model, program)
	return body, nil
}

type daybreakAttempt struct {
	ctx      context.Context
	account  *auth.Account
	body     []byte
	identity string
}

func (attempt daybreakAttempt) observe(response *http.Response) {
	if response == nil || response.Body == nil {
		return
	}
	program := gjson.GetBytes(attempt.body, "access_programs.cyber").String()
	if program != auth.DaybreakBlue && program != auth.DaybreakRed {
		return
	}
	if response.StatusCode < http.StatusBadRequest {
		if strings.Contains(response.Header.Get("Content-Type"), "text/event-stream") {
			response.Body = &daybreakEventObserver{ReadCloser: response.Body, attempt: attempt}
		}
		return
	}
	const errorLimit = 64 << 10
	prefix, err := io.ReadAll(io.LimitReader(response.Body, errorLimit))
	response.Body = &daybreakReplayBody{Reader: io.MultiReader(bytes.NewReader(prefix), response.Body), Closer: response.Body}
	if err == nil {
		attempt.inspect(prefix)
	}
}

func (attempt daybreakAttempt) inspect(body []byte) {
	for _, path := range []string{"error.code", "response.error.code"} {
		if gjson.GetBytes(body, path).String() == "access_program_not_enabled" {
			attempt.revoke()
			return
		}
	}
}

func (attempt daybreakAttempt) revoke() {
	ctx, account, body := attempt.ctx, attempt.account, attempt.body
	if account.DaybreakIdentity() != attempt.identity {
		return
	}
	program := gjson.GetBytes(body, "access_programs.cyber").String()
	snapshot := account.DaybreakSnapshot()
	model := gjson.GetBytes(body, "model").String()
	var retained []string
	for _, value := range snapshot.Models[model] {
		if value != program {
			retained = append(retained, value)
		}
	}
	delete(snapshot.Models, model)
	if len(retained) > 0 {
		snapshot.Models[model] = retained
	}
	snapshot.ObservedAt = account.BeginDaybreakObservation().ObservedAt
	account.ApplyDaybreakSnapshot(snapshot)
	guard, _ := ctx.Value(daybreakContextKey{}).(daybreakGuard)
	if guard.DB == nil {
		return
	}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := guard.DB.SaveDaybreakSnapshot(writeCtx, account.ID(), snapshot); err != nil {
		log.Printf("[daybreak] account=%d persist revocation failed: %v", account.ID(), err)
	}
}

type daybreakReplayBody struct {
	io.Reader
	io.Closer
}
