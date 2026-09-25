package proxy

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/codex2api/api"
	"github.com/codex2api/database"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func mappingTestDatabase(t *testing.T, h *Handler) func() {
	t.Helper()
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "mapping-owned.db"))
	require.NoError(t, err)
	h.db = db
	var once sync.Once
	closeDB := func() { once.Do(func() { require.NoError(t, db.Close()) }) }
	t.Cleanup(closeDB)
	return closeDB
}

func TestResponseMappingSurvivesShortStorageStall(t *testing.T) {
	for _, kind := range []string{"response_id", "turn_state"} {
		t.Run(kind, func(t *testing.T) {
			h, owner, _, _ := responsePrivacySetup(t)
			path := filepath.Join(t.TempDir(), "mapping.db")
			db, err := database.New("sqlite", path)
			require.NoError(t, err)
			defer db.Close()
			h.db = db
			c, _, _ := responsePrivacyRequest(t, h, 101, "stall", "")
			// A separate connection holds the SQLite writer lock beyond the old
			// one-second deadline. No upstream retry is involved in recovery.
			lockDB, err := sql.Open("sqlite", path)
			require.NoError(t, err)
			defer lockDB.Close()
			conn, err := lockDB.Conn(t.Context())
			require.NoError(t, err)
			defer conn.Close()
			_, err = conn.ExecContext(t.Context(), "BEGIN IMMEDIATE")
			require.NoError(t, err)
			done := make(chan error, 1)
			go func() {
				if kind == "response_id" {
					_, err := responseIdentityFrom(c.Request.Context()).issue(c.Request.Context(), owner, "resp_stall")
					done <- err
				} else {
					_, err := turnStateSessionFrom(c.Request.Context()).issue(c.Request.Context(), owner, "state_stall", "response_header")
					done <- err
				}
			}()
			time.Sleep(1200 * time.Millisecond)
			_, err = conn.ExecContext(t.Context(), "ROLLBACK")
			require.NoError(t, err)
			require.NoError(t, <-done)
			require.Empty(t, responseMappingDiagnostics(c.Request.Context()))
		})
	}
}

func TestResponseMappingScopedCacheAndIdempotence(t *testing.T) {
	h, owner, target, _ := responsePrivacySetup(t)
	closeDB := mappingTestDatabase(t, h)
	c, _, _ := responsePrivacyRequest(t, h, 101, "cache", "")
	ctx := c.Request.Context()
	db, binding := protocolIdentityBinding(ctx, owner)
	pair := database.CodexProtocolPair{Public: "local-turn", Upstream: "upstream-turn"}
	_, found, err := readResponseProtocolPair(ctx, db, binding, "turn", pair.Upstream, false)
	require.NoError(t, err)
	require.False(t, found)
	require.NoError(t, db.PutCodexProtocolPair(ctx, binding, "turn", pair))
	read, found, err := readResponseProtocolPair(ctx, db, binding, "turn", pair.Upstream, false)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, pair, read)
	_, other := protocolIdentityBinding(ctx, target)
	_, found, err = readResponseProtocolPair(ctx, db, other, "turn", pair.Upstream, false)
	require.NoError(t, err)
	require.False(t, found)
	alias, err := responseIdentityFrom(ctx).issue(ctx, owner, "resp_cache")
	require.NoError(t, err)
	again, err := responseIdentityFrom(ctx).issue(ctx, owner, alias)
	require.NoError(t, err)
	require.Equal(t, alias, again)
	// A previously persisted pair remains usable through repeated frames even
	// if storage goes down. Unknown or foreign pairs still fail closed.
	closeDB()
	read, found, err = readResponseProtocolPair(ctx, db, binding, "turn", pair.Public, true)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, pair, read)
	other.Generation++
	_, _, err = readResponseProtocolPair(ctx, db, other, "turn", pair.Upstream, false)
	require.ErrorIs(t, err, errTurnStateMapping)
	require.Equal(t, "turn_read", responseMappingDiagnostics(ctx)[0].Operation)
}

func TestResponseMappingStreamFailureIsLocal(t *testing.T) {
	h, owner, _, _ := responsePrivacySetup(t)
	closeDB := mappingTestDatabase(t, h)
	c, _, _ := responsePrivacyRequest(t, h, 101, "failure", "")
	closeDB()
	response := &http.Response{Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_do_not_expose\"}}\n\n"))}
	require.NoError(t, maskTurnStateResponse(c.Request.Context(), owner, response))
	body, err := io.ReadAll(response.Body)
	require.ErrorIs(t, err, errTurnStateMapping)
	require.Empty(t, body)
	outcome := classifyStreamOutcome(nil, err, nil, false)
	require.True(t, outcome.terminalLocal)
	require.False(t, outcome.penalize)
	require.False(t, isActualUpstreamStreamFailure(outcome, ""))
	require.Equal(t, http.StatusInternalServerError, outcome.logStatusCode)
	require.Equal(t, "response_mapping", outcome.failureKind)
	d := responseMappingDiagnostics(c.Request.Context())
	require.Len(t, d, 1)
	require.Equal(t, "response_id_write", d[0].Operation)
	require.Equal(t, responseMappingFailureMessage, localResponseFailureMessage(c))
	encoded, err := json.Marshal(d)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "resp_do_not_expose")
}

func TestResponseMappingHonorsCancellationAndHidesDriverText(t *testing.T) {
	h, owner, _, _ := responsePrivacySetup(t)
	c, _, _ := responsePrivacyRequest(t, h, 101, "canceled", "")
	ctx, cancel := context.WithCancel(c.Request.Context())
	cancel()
	_, err := responseIdentityFrom(ctx).issue(ctx, owner, "resp_cancel")
	require.ErrorIs(t, err, context.Canceled)
	require.ErrorIs(t, err, errTurnStateMapping)
	require.Equal(t, "canceled", responseMappingDiagnostics(ctx)[0].Reason)
	// A local DB deadline must not look like an upstream transport timeout,
	// including when an adapter wraps it in a retryable upstream error.
	mappingTimeout := responseMappingFailure(ctx, "response_id_write", context.DeadlineExceeded, time.Now())
	for _, wrapped := range []error{mappingTimeout, ErrInternalError("mapping failed", mappingTimeout), ErrUpstream(500, "mapping failed", mappingTimeout)} {
		require.Empty(t, classifyTransportFailure(wrapped))
		retries := 0
		require.False(t, shouldRetryRequestError(wrapped, &retries, 2))
		require.Zero(t, retries)
	}
	for i := 0; i < 30; i++ {
		_ = responseMappingFailure(ctx, "metadata_read", errors.New("driver secret=should-not-leak"), time.Now())
	}
	d := responseMappingDiagnostics(ctx)
	require.Len(t, d, 16)
	encoded, err := json.Marshal(d)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "should-not-leak")
}

type mappingSQLTestError struct{}

func (mappingSQLTestError) Error() string    { return "database secret=must-not-be-logged" }
func (mappingSQLTestError) SQLState() string { return "42P01" }

func TestResponseMappingFailureInUsageAndServiceLogs(t *testing.T) {
	h, _, _, _ := responsePrivacySetup(t)
	c, _, _ := responsePrivacyRequest(t, h, 101, "logging", "")
	finish := h.beginServiceErrorAudit(c)
	_ = responseMappingFailure(c.Request.Context(), "metadata_read", mappingSQLTestError{}, time.Now())
	input := &database.UsageLogInput{StatusCode: 500, UpstreamDiagnostics: `{"transport":"http","http_status":200,"error_source":"upstream_stream_or_transport"}`}
	populateUsageRequestDiagnostics(c, input)
	require.Equal(t, "42P01", gjson.Get(input.RequestDiagnostics, "response_mapping.0.sql_state").String())
	require.Equal(t, "gateway", gjson.Get(input.RequestDiagnostics, "upstream.error_source").String())
	require.Equal(t, "response_mapping", gjson.Get(input.RequestDiagnostics, "upstream.error_stage").String())
	require.NotContains(t, input.RequestDiagnostics, "must-not-be-logged")
	api.SendError(c, api.NewAPIError(api.ErrCodeServerError, responseMappingFailureMessage, api.ErrorTypeServer))
	finish()
	page := serviceErrorTestPage(t, h)
	require.Len(t, page.Items, 1)
	require.Len(t, page.Items[0].ResponseMapping, 1)
	require.Equal(t, "42P01", page.Items[0].ResponseMapping[0].SQLState)
	require.Equal(t, "response_mapping", gjson.GetBytes(page.Items[0].UpstreamInfo, "error_stage").String())
}
