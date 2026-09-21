package proxy

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
)

type syntheticTurnStateKey struct{}

// Native clients already bind a scoped session. Administrator probes get an
// isolated per-test scope using the same DB/key, never an upstream placeholder.
func prepareBPSSyntheticResponse(ctx context.Context, account *auth.Account, resp *http.Response) (context.Context, error) {
	if CodexBPSResponseDiagnostic(resp) == nil || resp.StatusCode < 200 || resp.StatusCode >= 300 || resp.Header.Get(codexTurnStateHeader) != "" {
		return ctx, nil
	}
	d := CodexBPSResponseDiagnostic(resp)
	real := measureUsageTurnState(resp.Header.Get(codexTurnStateHeader))
	d.UpstreamTurnState = &real
	ctx = context.WithValue(ctx, codexBPSDiagnosticKey{}, d)
	s := turnStateSessionFrom(ctx)
	if s == nil {
		db, _ := ctx.Value(codexIdentityClaimerContextKey{}).(*database.DB)
		if db == nil {
			return ctx, nil
		}
		key := NewUpstreamSessionUUID()
		s = &turnStateSession{handler: &Handler{db: db}, scope: codexIdentityDigest("bps-test-turn", key), rootKey: hashRiskIdentity(key), incoming: make(map[string]database.CodexTurnStateRecord), issued: make(map[string]database.CodexTurnStateRecord)}
		ctx = context.WithValue(ctx, turnStateSessionKey{}, s)
	}
	// Kind is part of the authenticated DB binding. This internal source marker
	// is never restored, used for routing, or written as an upstream observation.
	alias, err := s.issueKind(ctx, account, s.syntheticSource(ctx, account), "response_header."+codexTurnStateHeader, database.CodexTurnStateSyntheticBPS)
	if err != nil {
		return ctx, err
	}
	if alias == "" {
		return ctx, nil
	}
	resp.Header.Set(codexTurnStateHeader, alias)
	value := measureUsageTurnState(alias)
	value.Source = "synthetic"
	if d := CodexBPSResponseDiagnostic(resp); d != nil {
		d.ClientTurnState = &value
	}
	UpstreamTransportObserver(ctx).update(func(d *UpstreamTransportDiagnostic) { d.ClientTurnState = &value })
	return context.WithValue(ctx, syntheticTurnStateKey{}, alias), nil
}

func (s *turnStateSession) syntheticSource(ctx context.Context, account *auth.Account) string {
	const source = "local-bps-turn-state-v1"
	if s.turnKnown {
		return source
	}
	// Legacy clients without a turn ID cannot prove that two independent calls
	// are the same turn. Only an authenticated echo (or this attempt's issued
	// value) reuses its source; otherwise mint an independent local token.
	generation := uint64(0)
	if epoch := outboundEpochFromContext(ctx); epoch != nil {
		generation = epoch.record.FailoverCount
	}
	valid := func(record database.CodexTurnStateRecord) bool {
		return record.Kind == database.CodexTurnStateSyntheticBPS && record.Scope == s.scope && record.AccountID == account.ID() && record.AccountHash == turnStateAccountHash(account) && record.Generation == generation
	}
	s.mu.Lock()
	for _, record := range s.issued {
		if valid(record) {
			s.mu.Unlock()
			return record.Real
		}
	}
	s.mu.Unlock()
	for _, record := range s.incoming {
		if valid(record) {
			return record.Real
		}
	}
	return source + ":" + NewUpstreamSessionUUID()
}

func syntheticTurnStateFrame(ctx context.Context) []byte {
	value, _ := ctx.Value(syntheticTurnStateKey{}).(string)
	if value == "" {
		return nil
	}
	data, _ := json.Marshal(map[string]any{"type": "response.metadata", "headers": map[string]string{codexTurnStateHeader: value}})
	return append(append([]byte("data: "), data...), '\n', '\n')
}
