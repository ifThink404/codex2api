package proxy

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/codex2api/database"
)

// Mapping is local durable work, not another inference attempt. Allow a short
// storage stall without severing a healthy stream; the request deadline still
// wins. Never publish an alias that was not successfully persisted.
const responseMappingTimeout = 5 * time.Second

const responseMappingFailureMessage = "Internal proxy response mapping failure"

type responseMappingError struct{ cause error }

func (e *responseMappingError) Error() string        { return errTurnStateMapping.Error() }
func (e *responseMappingError) Unwrap() error        { return e.cause }
func (e *responseMappingError) Is(target error) bool { return target == errTurnStateMapping }

// Cancellation may arrive while a metadata lookup is running, including when
// the bounded usage drain expires. It is not a storage failure and must not
// override client cancellation or a terminal event whose usage was collected.
func canceledResponseMapping(err error) bool {
	return errors.Is(err, errTurnStateMapping) && errors.Is(err, context.Canceled)
}

func hasLocalResponseMappingFailure(failures []database.ResponseMappingFailure) bool {
	for _, failure := range failures {
		if failure.Reason != "canceled" {
			return true
		}
	}
	return false
}

func annotateResponseMappingFailure(upstream *UpstreamTransportDiagnostic, failures []database.ResponseMappingFailure, status int) {
	if upstream == nil || status < 400 || len(failures) == 0 {
		return
	}
	if hasLocalResponseMappingFailure(failures) {
		upstream.ErrorSource, upstream.ErrorStage = "gateway", "response_mapping"
	} else if status == logStatusClientClosed {
		upstream.ErrorSource, upstream.ErrorStage = "downstream", "request_canceled"
	}
}

func protocolIdentitySession(ctx context.Context) *responseIdentitySession {
	if ctx == nil {
		return nil
	}
	if s, _ := ctx.Value(protocolIdentityKey{}).(*responseIdentitySession); s != nil {
		return s
	}
	return responseIdentityFrom(ctx)
}

func responseMappingFailure(ctx context.Context, operation string, err error, started time.Time) error {
	d := database.ResponseMappingFailure{Operation: operation, Reason: "storage_error"}
	if !started.IsZero() {
		d.DurationMs = time.Since(started).Milliseconds()
	}
	switch {
	case errors.Is(err, context.Canceled):
		d.Reason = "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		d.Reason = "timeout"
	case errors.Is(err, database.ErrCodexIdentityAliasCollision):
		d.Reason = "alias_collision"
	case err == nil:
		d.Reason = "mapping_not_found"
	default:
		// Do not log driver error text: it may contain identifiers, ciphertext,
		// SQL arguments or connection credentials. SQLSTATE is sufficient to
		// distinguish missing schema, locks and database availability failures.
		var sqlState interface{ SQLState() string }
		if errors.As(err, &sqlState) {
			d.SQLState = sqlState.SQLState()
		}
		if strings.HasPrefix(err.Error(), "invalid ") {
			d.Reason = "invalid_mapping"
		}
		if err.Error() == "sql: database is closed" {
			d.Reason = "database_closed"
		}
	}
	if operation == "response_id_context" {
		d.Reason = "context_unavailable"
	}
	if operation == "response_id_capacity" {
		d.Reason = "request_mapping_limit"
	}
	if s := protocolIdentitySession(ctx); s != nil {
		s.mappingMu.Lock()
		if len(s.mappingFailures) < 16 {
			s.mappingFailures = append(s.mappingFailures, d)
		}
		s.mappingMu.Unlock()
	}
	return &responseMappingError{cause: err}
}

func responseMappingDiagnostics(ctx context.Context) []database.ResponseMappingFailure {
	s := protocolIdentitySession(ctx)
	if s == nil {
		return nil
	}
	s.mappingMu.Lock()
	defer s.mappingMu.Unlock()
	return append([]database.ResponseMappingFailure(nil), s.mappingFailures...)
}

func protocolPairCacheKey(binding database.CodexTurnStateBinding, kind, value string, public bool) string {
	return codexIdentityDigest(binding.Kind, binding.Scope, binding.RootKey, strconv.FormatInt(binding.AccountID, 10), binding.AccountHash, strconv.FormatUint(binding.Generation, 10), kind, strconv.FormatBool(public), value)
}

func cacheResponseProtocolPair(ctx context.Context, binding database.CodexTurnStateBinding, kind string, pair database.CodexProtocolPair) {
	s := protocolIdentitySession(ctx)
	if s == nil {
		return
	}
	s.mappingMu.Lock()
	defer s.mappingMu.Unlock()
	if s.protocolPairs == nil {
		s.protocolPairs = make(map[string]database.CodexProtocolPair)
	}
	if len(s.protocolPairs) >= 4096 {
		return
	}
	s.protocolPairs[protocolPairCacheKey(binding, kind, pair.Public, true)] = pair
	s.protocolPairs[protocolPairCacheKey(binding, kind, pair.Upstream, false)] = pair
}

// Cache successful authenticated lookups only. A miss must not hide a mapping
// created later in the same request. Scope/account/epoch are part of every key.
func readResponseProtocolPair(ctx context.Context, db *database.DB, binding database.CodexTurnStateBinding, kind, value string, public bool) (database.CodexProtocolPair, bool, error) {
	if s := protocolIdentitySession(ctx); s != nil {
		s.mappingMu.Lock()
		pair, ok := s.protocolPairs[protocolPairCacheKey(binding, kind, value, public)]
		s.mappingMu.Unlock()
		if ok {
			return pair, true, nil
		}
	}
	started := time.Now()
	lookup, cancel := context.WithTimeout(ctx, responseMappingTimeout)
	defer cancel()
	pair, found, err := db.ReadCodexProtocolPair(lookup, binding, kind, value, public)
	if err != nil {
		return pair, false, responseMappingFailure(ctx, kind+"_read", err, started)
	}
	if found {
		cacheResponseProtocolPair(ctx, binding, kind, pair)
	}
	return pair, found, nil
}

func putResponseProtocolPair(ctx context.Context, db *database.DB, binding database.CodexTurnStateBinding, kind string, pair database.CodexProtocolPair) error {
	if s := protocolIdentitySession(ctx); s != nil {
		s.mappingMu.Lock()
		existing, ok := s.protocolPairs[protocolPairCacheKey(binding, kind, pair.Public, true)]
		s.mappingMu.Unlock()
		if ok && existing == pair {
			return nil
		}
	}
	started := time.Now()
	saving, cancel := context.WithTimeout(ctx, responseMappingTimeout)
	defer cancel()
	if err := db.PutCodexProtocolPair(saving, binding, kind, pair); err != nil {
		return responseMappingFailure(ctx, kind+"_write", err, started)
	}
	cacheResponseProtocolPair(ctx, binding, kind, pair)
	return nil
}
