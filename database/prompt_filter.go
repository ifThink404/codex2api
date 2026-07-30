package database

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	promptFilterAuditHighCapacity = 512
	promptFilterAuditLowCapacity  = 3584
	promptFilterAuditWorkers      = 4
	promptFilterAuditTaskTimeout  = 3 * time.Second
	promptFilterAuditMaxJobBytes  = 256 * 1024
	promptFilterAuditMaxHighBytes = 8 * 1024 * 1024
	promptFilterAuditMaxLowBytes  = 24 * 1024 * 1024
)

type PromptFilterLogPriority uint8

const (
	PromptFilterLogPriorityLow PromptFilterLogPriority = iota
	PromptFilterLogPriorityHigh
)

type PromptFilterAuditStats struct {
	Enqueued           uint64
	Completed          uint64
	DroppedHigh        uint64
	DroppedLow         uint64
	Failed             uint64
	ProcessingNanos    uint64
	MaxProcessingNanos uint64
	PendingHigh        int
	PendingLow         int
	RetainedBytes      int64
}

type promptFilterAuditJob struct {
	input             PromptFilterLogInput
	hasLog            bool
	incident          PromptPolicyIncidentInput
	hasIncident       bool
	candidate         PromptRuleCandidateInput
	candidateEvidence PromptRuleCandidateEvidenceInput
	hasCandidate      bool
	bytes             int64
}

type promptFilterAuditQueue struct {
	db        *DB
	high      chan promptFilterAuditJob
	low       chan promptFilterAuditJob
	ctx       context.Context
	stop      chan struct{}
	done      chan struct{}
	cancel    context.CancelFunc
	closed    atomic.Bool
	enqueueMu sync.RWMutex
	wg        sync.WaitGroup

	enqueued           atomic.Uint64
	completed          atomic.Uint64
	droppedHigh        atomic.Uint64
	droppedLow         atomic.Uint64
	failed             atomic.Uint64
	processingNanos    atomic.Uint64
	maxProcessingNanos atomic.Uint64
	pending            atomic.Int64
	retainedHigh       atomic.Int64
	retainedLow        atomic.Int64
	lastDropLog        atomic.Int64
}

func newPromptFilterAuditQueue(db *DB) *promptFilterAuditQueue {
	ctx, cancel := context.WithCancel(context.Background())
	queue := &promptFilterAuditQueue{
		db: db, high: make(chan promptFilterAuditJob, promptFilterAuditHighCapacity), low: make(chan promptFilterAuditJob, promptFilterAuditLowCapacity),
		ctx: ctx, stop: make(chan struct{}), done: make(chan struct{}), cancel: cancel,
	}
	return queue
}

func (q *promptFilterAuditQueue) start() {
	if q == nil || q.db == nil {
		return
	}
	q.wg.Add(promptFilterAuditWorkers)
	for range promptFilterAuditWorkers {
		go q.worker()
	}
	go func() {
		q.wg.Wait()
		close(q.done)
	}()
}

func (q *promptFilterAuditQueue) close(timeout time.Duration) {
	if q == nil {
		return
	}
	q.enqueueMu.Lock()
	if !q.closed.CompareAndSwap(false, true) {
		q.enqueueMu.Unlock()
		return
	}
	close(q.stop)
	q.enqueueMu.Unlock()
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-q.done:
		q.cancel()
		return
	case <-timer.C:
		q.cancel()
	}
	// Give canceled database calls a short, fixed window to unwind. Close may
	// continue afterwards; workers hold no request-scoped data or raw bodies.
	select {
	case <-q.done:
	case <-time.After(250 * time.Millisecond):
	}
}

func (q *promptFilterAuditQueue) enqueue(input PromptFilterLogInput, priority PromptFilterLogPriority) bool {
	if q == nil || q.db == nil {
		return false
	}
	jobBytes := int64(promptFilterLogInputBytes(input))
	if jobBytes > promptFilterAuditMaxJobBytes {
		q.drop(priority, "job_too_large")
		return false
	}
	input = clonePromptFilterLogInput(input)
	return q.enqueueJob(promptFilterAuditJob{input: input, hasLog: true, bytes: jobBytes}, priority)
}

func (q *promptFilterAuditQueue) enqueueCandidate(candidate PromptRuleCandidateInput, evidence PromptRuleCandidateEvidenceInput, priority PromptFilterLogPriority) bool {
	if q == nil || q.db == nil {
		return false
	}
	jobBytes := int64(promptRuleCandidateJobBytes(candidate, evidence))
	if jobBytes > promptFilterAuditMaxJobBytes {
		q.drop(priority, "job_too_large")
		return false
	}
	job := promptFilterAuditJob{
		candidate: clonePromptRuleCandidateInput(candidate), candidateEvidence: clonePromptRuleCandidateEvidenceInput(evidence),
		hasCandidate: true, bytes: jobBytes,
	}
	return q.enqueueJob(job, priority)
}

func (q *promptFilterAuditQueue) enqueueIncident(incident PromptPolicyIncidentInput, candidate PromptRuleCandidateInput, evidence PromptRuleCandidateEvidenceInput) bool {
	if q == nil || q.db == nil {
		return false
	}
	jobBytes := int64(promptPolicyIncidentJobBytes(incident, candidate, evidence))
	if jobBytes > promptFilterAuditMaxJobBytes {
		q.drop(PromptFilterLogPriorityHigh, "job_too_large")
		return false
	}
	job := promptFilterAuditJob{
		incident: clonePromptPolicyIncidentInput(incident), hasIncident: true,
		candidate: clonePromptRuleCandidateInput(candidate), candidateEvidence: clonePromptRuleCandidateEvidenceInput(evidence),
		hasCandidate: true, bytes: jobBytes,
	}
	return q.enqueueJob(job, PromptFilterLogPriorityHigh)
}

func (q *promptFilterAuditQueue) enqueueJob(job promptFilterAuditJob, priority PromptFilterLogPriority) bool {
	q.enqueueMu.RLock()
	defer q.enqueueMu.RUnlock()
	if q.closed.Load() {
		q.drop(priority, "closed")
		return false
	}
	if !q.reserveBytes(priority, job.bytes) {
		q.drop(priority, "queue_bytes_full")
		return false
	}
	queue := q.low
	if priority == PromptFilterLogPriorityHigh {
		queue = q.high
	}
	q.pending.Add(1)
	select {
	case <-q.stop:
		q.pending.Add(-1)
		q.releaseBytes(priority, job.bytes)
		q.drop(priority, "closed")
		return false
	case queue <- job:
		q.enqueued.Add(1)
		return true
	default:
		q.pending.Add(-1)
		q.releaseBytes(priority, job.bytes)
		q.drop(priority, "queue_full")
		return false
	}
}

func (q *promptFilterAuditQueue) reserveBytes(priority PromptFilterLogPriority, size int64) bool {
	limit := int64(promptFilterAuditMaxLowBytes)
	counter := &q.retainedLow
	if priority == PromptFilterLogPriorityHigh {
		limit = promptFilterAuditMaxHighBytes
		counter = &q.retainedHigh
	}
	if size < 0 || size > limit {
		return false
	}
	for {
		current := counter.Load()
		if current+size > limit {
			return false
		}
		if counter.CompareAndSwap(current, current+size) {
			return true
		}
	}
}

func (q *promptFilterAuditQueue) releaseBytes(priority PromptFilterLogPriority, size int64) {
	if priority == PromptFilterLogPriorityHigh {
		q.retainedHigh.Add(-size)
		return
	}
	q.retainedLow.Add(-size)
}

func (q *promptFilterAuditQueue) worker() {
	defer q.wg.Done()
	for {
		job, priority, ok := q.next()
		if !ok {
			return
		}
		func() {
			started := time.Now()
			defer q.pending.Add(-1)
			defer q.releaseBytes(priority, job.bytes)
			defer func() {
				elapsed := uint64(time.Since(started))
				q.processingNanos.Add(elapsed)
				for {
					current := q.maxProcessingNanos.Load()
					if elapsed <= current || q.maxProcessingNanos.CompareAndSwap(current, elapsed) {
						break
					}
				}
			}()
			defer func() {
				if recovered := recover(); recovered != nil {
					q.failed.Add(1)
					log.Printf("prompt filter audit worker panic: %v", recovered)
				}
			}()
			attempts := 1
			if priority == PromptFilterLogPriorityHigh {
				attempts = 2
			}
			for attempt := 0; attempt < attempts; attempt++ {
				ctx, cancel := context.WithTimeout(q.ctx, promptFilterAuditTaskTimeout)
				var err error
				if job.hasIncident {
					err = q.db.PersistPromptPolicyIncident(ctx, job.incident, job.candidate, job.candidateEvidence)
				} else if job.hasLog {
					err = q.db.InsertPromptFilterLog(ctx, &job.input)
				} else if job.hasCandidate {
					_, _, err = q.db.StagePromptRuleCandidate(ctx, job.candidate, job.candidateEvidence)
				}
				cancel()
				if err == nil {
					q.completed.Add(1)
					return
				}
				if attempt+1 < attempts {
					select {
					case <-q.ctx.Done():
						break
					case <-time.After(25 * time.Millisecond):
						continue
					}
				}
				q.failed.Add(1)
				log.Printf("prompt filter audit persist failed: %v", err)
				return
			}
		}()
	}
}

func (q *promptFilterAuditQueue) next() (promptFilterAuditJob, PromptFilterLogPriority, bool) {
	select {
	case input := <-q.high:
		return input, PromptFilterLogPriorityHigh, true
	default:
	}
	select {
	case input := <-q.high:
		return input, PromptFilterLogPriorityHigh, true
	case input := <-q.low:
		return input, PromptFilterLogPriorityLow, true
	case <-q.stop:
		return q.nextDraining()
	}
}

func (q *promptFilterAuditQueue) nextDraining() (promptFilterAuditJob, PromptFilterLogPriority, bool) {
	select {
	case input := <-q.high:
		return input, PromptFilterLogPriorityHigh, true
	default:
	}
	select {
	case input := <-q.high:
		return input, PromptFilterLogPriorityHigh, true
	case input := <-q.low:
		return input, PromptFilterLogPriorityLow, true
	default:
		return promptFilterAuditJob{}, PromptFilterLogPriorityLow, false
	}
}

func (q *promptFilterAuditQueue) drop(priority PromptFilterLogPriority, reason string) {
	if priority == PromptFilterLogPriorityHigh {
		q.droppedHigh.Add(1)
	} else {
		q.droppedLow.Add(1)
	}
	now := time.Now().UnixNano()
	last := q.lastDropLog.Load()
	if last != 0 && time.Duration(now-last) < 5*time.Second {
		return
	}
	if q.lastDropLog.CompareAndSwap(last, now) {
		log.Printf("prompt filter audit dropped: reason=%s priority_high=%t dropped_high=%d dropped_low=%d", reason, priority == PromptFilterLogPriorityHigh, q.droppedHigh.Load(), q.droppedLow.Load())
	}
}

func promptFilterLogInputBytes(input PromptFilterLogInput) int {
	return len(input.Source) + len(input.Endpoint) + len(input.Protocol) + len(input.Provider) + len(input.Model) +
		len(input.Action) + len(input.Mode) + len(input.PolicyProfile) + len(input.ReasonCode) + len(input.PrimaryOrigin) +
		len(input.MatchedPatterns) + len(input.TextPreview) + len(input.MatchContext) + len(input.FullText) +
		len(input.APIKeyName) + len(input.APIKeyMasked) + len(input.ClientIP) + len(input.ErrorCode) + len(input.ReviewModel) + len(input.ReviewError) +
		len(input.RequestCorrelationID)
}

func clonePromptFilterLogInput(input PromptFilterLogInput) PromptFilterLogInput {
	input.Source = strings.Clone(input.Source)
	input.Endpoint = strings.Clone(input.Endpoint)
	input.Protocol = strings.Clone(input.Protocol)
	input.Provider = strings.Clone(input.Provider)
	input.Model = strings.Clone(input.Model)
	input.Action = strings.Clone(input.Action)
	input.Mode = strings.Clone(input.Mode)
	input.PolicyProfile = strings.Clone(input.PolicyProfile)
	input.ReasonCode = strings.Clone(input.ReasonCode)
	input.PrimaryOrigin = strings.Clone(input.PrimaryOrigin)
	input.MatchedPatterns = strings.Clone(input.MatchedPatterns)
	input.TextPreview = strings.Clone(input.TextPreview)
	input.MatchContext = strings.Clone(input.MatchContext)
	input.FullText = strings.Clone(input.FullText)
	input.APIKeyName = strings.Clone(input.APIKeyName)
	input.APIKeyMasked = strings.Clone(input.APIKeyMasked)
	input.ClientIP = strings.Clone(input.ClientIP)
	input.ErrorCode = strings.Clone(input.ErrorCode)
	input.ReviewModel = strings.Clone(input.ReviewModel)
	input.ReviewError = strings.Clone(input.ReviewError)
	input.RequestCorrelationID = strings.Clone(input.RequestCorrelationID)
	return input
}

func promptRuleCandidateJobBytes(candidate PromptRuleCandidateInput, evidence PromptRuleCandidateEvidenceInput) int {
	return len(candidate.Fingerprint) + len(candidate.Kind) + len(candidate.Source) + len(candidate.Name) + len(candidate.Category) +
		len(candidate.RuleJSON) + len(candidate.Rationale) + len(candidate.SourceURL) + len(candidate.SamplePreview) +
		len(evidence.SourceKind) + len(evidence.SourceRef) + len(evidence.SourceRefHash) + len(evidence.SamplePreview) +
		len(evidence.MetadataJSON) + len(evidence.Protocol) + len(evidence.Provider) + len(evidence.Model) + len(evidence.APIKeyName) +
		len(evidence.PromptPolicyIncidentID)
}

func promptPolicyIncidentJobBytes(incident PromptPolicyIncidentInput, candidate PromptRuleCandidateInput, evidence PromptRuleCandidateEvidenceInput) int {
	return len(incident.IncidentID) + len(incident.RequestCorrelationID) + len(incident.Transport) + len(incident.Endpoint) +
		len(incident.Protocol) + len(incident.Provider) + len(incident.Model) + len(incident.APIKeyName) + len(incident.APIKeyMasked) +
		len(incident.Platform) + len(incident.SourceRef) + len(incident.UpstreamErrorCode) + len(incident.UpstreamError) +
		len(incident.LocalEvaluationState) + len(incident.LocalOutcome) + len(incident.LocalAction) + len(incident.LocalMode) +
		len(incident.LocalPolicyProfile) + len(incident.LocalReasonCode) + len(incident.LocalReason) + len(incident.LocalPrimaryOrigin) + len(incident.LocalReviewModel) +
		len(incident.LocalReviewError) + len(incident.LocalMatchedPatterns) + len(incident.PromptFingerprint) + len(incident.PromptPreview) +
		len(incident.PromptText) + promptRuleCandidateJobBytes(candidate, evidence)
}

func clonePromptRuleCandidateInput(input PromptRuleCandidateInput) PromptRuleCandidateInput {
	input.Fingerprint = strings.Clone(input.Fingerprint)
	input.Kind = strings.Clone(input.Kind)
	input.Source = strings.Clone(input.Source)
	input.Name = strings.Clone(input.Name)
	input.Category = strings.Clone(input.Category)
	input.RuleJSON = strings.Clone(input.RuleJSON)
	input.Rationale = strings.Clone(input.Rationale)
	input.SourceURL = strings.Clone(input.SourceURL)
	input.SamplePreview = strings.Clone(input.SamplePreview)
	return input
}

func clonePromptRuleCandidateEvidenceInput(input PromptRuleCandidateEvidenceInput) PromptRuleCandidateEvidenceInput {
	input.SourceKind = strings.Clone(input.SourceKind)
	input.SourceRef = strings.Clone(input.SourceRef)
	input.SourceRefHash = strings.Clone(input.SourceRefHash)
	input.SamplePreview = strings.Clone(input.SamplePreview)
	input.MetadataJSON = strings.Clone(input.MetadataJSON)
	input.Protocol = strings.Clone(input.Protocol)
	input.Provider = strings.Clone(input.Provider)
	input.Model = strings.Clone(input.Model)
	input.APIKeyName = strings.Clone(input.APIKeyName)
	input.PromptPolicyIncidentID = strings.Clone(input.PromptPolicyIncidentID)
	return input
}

func clonePromptPolicyIncidentInput(input PromptPolicyIncidentInput) PromptPolicyIncidentInput {
	input.IncidentID = strings.Clone(input.IncidentID)
	input.RequestCorrelationID = strings.Clone(input.RequestCorrelationID)
	input.Transport = strings.Clone(input.Transport)
	input.Endpoint = strings.Clone(input.Endpoint)
	input.Protocol = strings.Clone(input.Protocol)
	input.Provider = strings.Clone(input.Provider)
	input.Model = strings.Clone(input.Model)
	input.APIKeyName = strings.Clone(input.APIKeyName)
	input.APIKeyMasked = strings.Clone(input.APIKeyMasked)
	input.Platform = strings.Clone(input.Platform)
	input.SourceRef = strings.Clone(input.SourceRef)
	input.UpstreamErrorCode = strings.Clone(input.UpstreamErrorCode)
	input.UpstreamError = strings.Clone(input.UpstreamError)
	input.LocalEvaluationState = strings.Clone(input.LocalEvaluationState)
	input.LocalOutcome = strings.Clone(input.LocalOutcome)
	input.LocalAction = strings.Clone(input.LocalAction)
	input.LocalMode = strings.Clone(input.LocalMode)
	input.LocalPolicyProfile = strings.Clone(input.LocalPolicyProfile)
	input.LocalReasonCode = strings.Clone(input.LocalReasonCode)
	input.LocalReason = strings.Clone(input.LocalReason)
	input.LocalPrimaryOrigin = strings.Clone(input.LocalPrimaryOrigin)
	input.LocalReviewModel = strings.Clone(input.LocalReviewModel)
	input.LocalReviewError = strings.Clone(input.LocalReviewError)
	input.LocalMatchedPatterns = strings.Clone(input.LocalMatchedPatterns)
	input.PromptFingerprint = strings.Clone(input.PromptFingerprint)
	input.PromptPreview = strings.Clone(input.PromptPreview)
	input.PromptText = strings.Clone(input.PromptText)
	return input
}

// EnqueuePromptFilterLog moves an already-redacted audit record off the
// request path. Queue saturation or storage failure never changes the policy
// decision and never falls back to a synchronous database write.
func (db *DB) EnqueuePromptFilterLog(input *PromptFilterLogInput, priority PromptFilterLogPriority) bool {
	if db == nil || input == nil || db.promptFilterAudit == nil {
		return false
	}
	return db.promptFilterAudit.enqueue(*input, priority)
}

// EnqueuePromptRuleCandidate stages already-redacted learning evidence on the
// same bounded, non-blocking queue used by prompt audit logs. Saturation never
// falls back to a synchronous database write.
func (db *DB) EnqueuePromptRuleCandidate(candidate *PromptRuleCandidateInput, evidence *PromptRuleCandidateEvidenceInput, priority PromptFilterLogPriority) bool {
	if db == nil || candidate == nil || evidence == nil || db.promptFilterAudit == nil {
		return false
	}
	return db.promptFilterAudit.enqueueCandidate(*candidate, *evidence, priority)
}

// EnqueuePromptPolicyIncident atomically persists an upstream CY incident and
// its candidate evidence without blocking or changing the request outcome.
func (db *DB) EnqueuePromptPolicyIncident(incident *PromptPolicyIncidentInput, candidate *PromptRuleCandidateInput, evidence *PromptRuleCandidateEvidenceInput) bool {
	if db == nil || incident == nil || candidate == nil || evidence == nil || db.promptFilterAudit == nil {
		return false
	}
	return db.promptFilterAudit.enqueueIncident(*incident, *candidate, *evidence)
}

func (db *DB) PromptFilterAuditStats() PromptFilterAuditStats {
	if db == nil || db.promptFilterAudit == nil {
		return PromptFilterAuditStats{}
	}
	q := db.promptFilterAudit
	return PromptFilterAuditStats{
		Enqueued: q.enqueued.Load(), Completed: q.completed.Load(), DroppedHigh: q.droppedHigh.Load(), DroppedLow: q.droppedLow.Load(), Failed: q.failed.Load(),
		ProcessingNanos: q.processingNanos.Load(), MaxProcessingNanos: q.maxProcessingNanos.Load(),
		PendingHigh: len(q.high), PendingLow: len(q.low), RetainedBytes: q.retainedHigh.Load() + q.retainedLow.Load(),
	}
}

// WaitPromptFilterAuditIdle is intended for shutdown coordination and tests;
// request handlers must never wait for audit persistence.
func (db *DB) WaitPromptFilterAuditIdle(ctx context.Context) bool {
	if db == nil || db.promptFilterAudit == nil {
		return true
	}
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		if db.promptFilterAudit.pending.Load() == 0 {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
		}
	}
}

type PromptFilterLog struct {
	ID                   int64     `json:"id"`
	CreatedAt            time.Time `json:"created_at"`
	Source               string    `json:"source"`
	Endpoint             string    `json:"endpoint"`
	Protocol             string    `json:"protocol"`
	Provider             string    `json:"provider"`
	Model                string    `json:"model"`
	Action               string    `json:"action"`
	Mode                 string    `json:"mode"`
	Score                int       `json:"score"`
	AuditScore           int       `json:"audit_score"`
	Threshold            int       `json:"threshold"`
	PolicyProfile        string    `json:"policy_profile"`
	ReasonCode           string    `json:"reason_code"`
	PrimaryOrigin        string    `json:"primary_origin"`
	StrikeEligible       bool      `json:"strike_eligible"`
	MatchedPatterns      string    `json:"matched_patterns"`
	TextPreview          string    `json:"text_preview"`
	MatchContext         string    `json:"match_context"`
	FullText             string    `json:"full_text"`
	APIKeyID             int64     `json:"api_key_id"`
	APIKeyName           string    `json:"api_key_name"`
	APIKeyMasked         string    `json:"api_key_masked"`
	ClientIP             string    `json:"client_ip"`
	ErrorCode            string    `json:"error_code"`
	ReviewModel          string    `json:"review_model"`
	ReviewFlagged        bool      `json:"review_flagged"`
	ReviewError          string    `json:"review_error"`
	RequestCorrelationID string    `json:"request_correlation_id,omitempty"`
}

type PromptFilterLogInput struct {
	Source               string
	Endpoint             string
	Protocol             string
	Provider             string
	Model                string
	Action               string
	Mode                 string
	Score                int
	AuditScore           int
	Threshold            int
	PolicyProfile        string
	ReasonCode           string
	PrimaryOrigin        string
	StrikeEligible       bool
	MatchedPatterns      string
	TextPreview          string
	MatchContext         string
	FullText             string
	APIKeyID             int64
	APIKeyName           string
	APIKeyMasked         string
	ClientIP             string
	ErrorCode            string
	ReviewModel          string
	ReviewFlagged        bool
	ReviewError          string
	RequestCorrelationID string
}

type PromptFilterLogQuery struct {
	Page     int
	PageSize int
	Limit    int
	Source   string
	Action   string
	Endpoint string
	Model    string
	APIKeyID int64
	Query    string
}

func (db *DB) InsertPromptFilterLog(ctx context.Context, input *PromptFilterLogInput) error {
	if db == nil || input == nil {
		return nil
	}
	_, err := db.conn.ExecContext(ctx, `
		INSERT INTO prompt_filter_logs (
			source, endpoint, request_protocol, request_provider, model, action, mode, score, audit_score, threshold_value, policy_profile, reason_code, primary_origin, strike_eligible, matched_patterns, text_preview,
			match_context, api_key_id, api_key_name, api_key_masked, client_ip, error_code, review_model, review_flagged, review_error, full_text, request_correlation_id
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24, $25, $26, $27)
	`, input.Source, input.Endpoint, input.Protocol, input.Provider, input.Model, input.Action, input.Mode, input.Score, input.AuditScore, input.Threshold,
		input.PolicyProfile, input.ReasonCode, input.PrimaryOrigin, input.StrikeEligible, input.MatchedPatterns, input.TextPreview, input.MatchContext,
		input.APIKeyID, input.APIKeyName, input.APIKeyMasked, input.ClientIP, input.ErrorCode, input.ReviewModel, input.ReviewFlagged, input.ReviewError, input.FullText,
		input.RequestCorrelationID)
	return err
}

func (db *DB) ListPromptFilterLogs(ctx context.Context, limit int) ([]*PromptFilterLog, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	result, _, err := db.ListPromptFilterLogsPage(ctx, PromptFilterLogQuery{Page: 1, PageSize: limit})
	return result, err
}

func (db *DB) ListPromptFilterLogsPage(ctx context.Context, query PromptFilterLogQuery) ([]*PromptFilterLog, int, error) {
	pageSize := query.PageSize
	if pageSize <= 0 {
		pageSize = query.Limit
	}
	if pageSize <= 0 || pageSize > 500 {
		pageSize = 100
	}
	page := query.Page
	if page <= 0 {
		page = 1
	}

	where, args := promptFilterLogWhere(query)
	countSQL := `SELECT COUNT(*) FROM prompt_filter_logs` + where
	var total int
	if err := db.conn.QueryRowContext(ctx, countSQL, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	args = append(args, pageSize, (page-1)*pageSize)
	rows, err := db.conn.QueryContext(ctx, `
		SELECT id, created_at, COALESCE(source, ''), COALESCE(endpoint, ''), COALESCE(request_protocol, ''), COALESCE(request_provider, ''), COALESCE(model, ''),
		       COALESCE(action, ''), COALESCE(mode, ''), COALESCE(score, 0), COALESCE(audit_score, 0), COALESCE(threshold_value, 0),
		       COALESCE(policy_profile, ''), COALESCE(reason_code, ''), COALESCE(primary_origin, ''), COALESCE(strike_eligible, false),
		       COALESCE(matched_patterns, '[]'), COALESCE(text_preview, ''), COALESCE(match_context, ''), COALESCE(api_key_id, 0),
		       COALESCE(api_key_name, ''), COALESCE(api_key_masked, ''), COALESCE(client_ip, ''), COALESCE(error_code, ''),
		       COALESCE(review_model, ''), COALESCE(review_flagged, false), COALESCE(review_error, ''), COALESCE(full_text, ''),
		       COALESCE(request_correlation_id, '')
		FROM prompt_filter_logs
		`+where+`
		ORDER BY id DESC
		LIMIT $`+fmt.Sprint(len(args)-1)+` OFFSET $`+fmt.Sprint(len(args))+`
	`, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	logs := make([]*PromptFilterLog, 0)
	for rows.Next() {
		item := &PromptFilterLog{}
		var createdAtRaw interface{}
		if err := rows.Scan(&item.ID, &createdAtRaw, &item.Source, &item.Endpoint, &item.Protocol, &item.Provider, &item.Model, &item.Action, &item.Mode,
			&item.Score, &item.AuditScore, &item.Threshold, &item.PolicyProfile, &item.ReasonCode, &item.PrimaryOrigin, &item.StrikeEligible,
			&item.MatchedPatterns, &item.TextPreview, &item.MatchContext, &item.APIKeyID, &item.APIKeyName,
			&item.APIKeyMasked, &item.ClientIP, &item.ErrorCode, &item.ReviewModel, &item.ReviewFlagged, &item.ReviewError, &item.FullText,
			&item.RequestCorrelationID); err != nil {
			return nil, 0, err
		}
		createdAt, err := parseDBTimeValue(createdAtRaw)
		if err != nil {
			return nil, 0, err
		}
		item.CreatedAt = createdAt
		logs = append(logs, item)
	}
	return logs, total, rows.Err()
}

func promptFilterLogWhere(query PromptFilterLogQuery) (string, []any) {
	clauses := make([]string, 0, 8)
	args := make([]any, 0, 8)
	addExact := func(column, value string) {
		value = strings.TrimSpace(value)
		if value == "" || value == "all" {
			return
		}
		args = append(args, value)
		clauses = append(clauses, fmt.Sprintf("%s = $%d", column, len(args)))
	}
	addExact("source", query.Source)
	addExact("action", query.Action)
	addExact("endpoint", query.Endpoint)
	addExact("model", query.Model)
	if query.APIKeyID > 0 {
		args = append(args, query.APIKeyID)
		clauses = append(clauses, fmt.Sprintf("api_key_id = $%d", len(args)))
	}
	if q := strings.TrimSpace(query.Query); q != "" {
		args = append(args, "%"+strings.ToLower(q)+"%")
		idx := len(args)
		clauses = append(clauses, fmt.Sprintf(`(
			LOWER(COALESCE(text_preview, '')) LIKE $%d OR
			LOWER(COALESCE(match_context, '')) LIKE $%d OR
			LOWER(COALESCE(full_text, '')) LIKE $%d OR
			LOWER(COALESCE(matched_patterns, '')) LIKE $%d OR
			LOWER(COALESCE(error_code, '')) LIKE $%d OR
			LOWER(COALESCE(review_error, '')) LIKE $%d OR
			LOWER(COALESCE(api_key_name, '')) LIKE $%d OR
			LOWER(COALESCE(api_key_masked, '')) LIKE $%d
		)`, idx, idx, idx, idx, idx, idx, idx, idx))
	}
	if len(clauses) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(clauses, " AND "), args
}

// FindNearestPromptFilterLog 返回与给定时间 at 最接近的一条提示词过滤日志，用于把
// 「使用统计」里的某次报错关联到对应的拦截记录（含完整请求内容）。按 source /
// api_key_id 过滤，时间窗口内取最接近的一条；endpoint 仅作为同等时间下的优先项。
func (db *DB) FindNearestPromptFilterLog(ctx context.Context, at time.Time, source, endpoint string, apiKeyID int64, windowSeconds int) (*PromptFilterLog, error) {
	if db == nil {
		return nil, nil
	}
	if windowSeconds <= 0 {
		windowSeconds = 10
	}
	startArg, endArg := db.timeRangeArgs(at.Add(-time.Duration(windowSeconds)*time.Second), at.Add(time.Duration(windowSeconds)*time.Second))
	clauses := []string{"created_at >= $1", "created_at <= $2"}
	args := []any{startArg, endArg}
	if s := strings.TrimSpace(source); s != "" {
		args = append(args, s)
		clauses = append(clauses, fmt.Sprintf("source = $%d", len(args)))
	}
	if apiKeyID > 0 {
		args = append(args, apiKeyID)
		clauses = append(clauses, fmt.Sprintf("api_key_id = $%d", len(args)))
	}

	rows, err := db.conn.QueryContext(ctx, `
		SELECT id, created_at, COALESCE(source, ''), COALESCE(endpoint, ''), COALESCE(request_protocol, ''), COALESCE(request_provider, ''), COALESCE(model, ''),
		       COALESCE(action, ''), COALESCE(mode, ''), COALESCE(score, 0), COALESCE(audit_score, 0), COALESCE(threshold_value, 0),
		       COALESCE(policy_profile, ''), COALESCE(reason_code, ''), COALESCE(primary_origin, ''), COALESCE(strike_eligible, false),
		       COALESCE(matched_patterns, '[]'), COALESCE(text_preview, ''), COALESCE(match_context, ''), COALESCE(api_key_id, 0),
		       COALESCE(api_key_name, ''), COALESCE(api_key_masked, ''), COALESCE(client_ip, ''), COALESCE(error_code, ''),
		       COALESCE(review_model, ''), COALESCE(review_flagged, false), COALESCE(review_error, ''), COALESCE(full_text, ''),
		       COALESCE(request_correlation_id, '')
		FROM prompt_filter_logs
		WHERE `+strings.Join(clauses, " AND ")+`
		ORDER BY id DESC
		LIMIT 50
	`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var best *PromptFilterLog
	var bestDelta time.Duration
	for rows.Next() {
		item := &PromptFilterLog{}
		var createdAtRaw interface{}
		if err := rows.Scan(&item.ID, &createdAtRaw, &item.Source, &item.Endpoint, &item.Protocol, &item.Provider, &item.Model, &item.Action, &item.Mode,
			&item.Score, &item.AuditScore, &item.Threshold, &item.PolicyProfile, &item.ReasonCode, &item.PrimaryOrigin, &item.StrikeEligible,
			&item.MatchedPatterns, &item.TextPreview, &item.MatchContext, &item.APIKeyID, &item.APIKeyName,
			&item.APIKeyMasked, &item.ClientIP, &item.ErrorCode, &item.ReviewModel, &item.ReviewFlagged, &item.ReviewError, &item.FullText,
			&item.RequestCorrelationID); err != nil {
			return nil, err
		}
		createdAt, err := parseDBTimeValue(createdAtRaw)
		if err != nil {
			continue
		}
		item.CreatedAt = createdAt
		delta := at.Sub(createdAt)
		if delta < 0 {
			delta = -delta
		}
		// endpoint 一致时给一点优先（减小有效距离），保证同一时刻多条时选对端点。
		if endpoint != "" && item.Endpoint == endpoint {
			if delta >= time.Second {
				delta -= time.Second
			} else {
				delta = 0
			}
		}
		if best == nil || delta < bestDelta {
			best = item
			bestDelta = delta
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return best, nil
}

func (db *DB) ClearPromptFilterLogs(ctx context.Context) error {
	if db == nil {
		return nil
	}
	if db.isSQLite() {
		if _, err := db.conn.ExecContext(ctx, `DELETE FROM prompt_filter_logs; DELETE FROM prompt_policy_incidents`); err != nil {
			return err
		}
		_, err := db.conn.ExecContext(ctx, `DELETE FROM sqlite_sequence WHERE name IN ('prompt_filter_logs', 'prompt_policy_incidents')`)
		return err
	}
	_, err := db.conn.ExecContext(ctx, `TRUNCATE TABLE prompt_filter_logs, prompt_policy_incidents RESTART IDENTITY`)
	return err
}
