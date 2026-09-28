package promptfilter

import "strings"

// LocalMode caps only deterministic local decisions, before external review.
// The global mode still caps the final result, including the reviewer.
func NormalizeLocalMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case ModeMonitor:
		return ModeMonitor
	case ModeWarn:
		return ModeWarn
	default:
		return ModeBlock
	}
}

func ApplyLocalMode(verdict Verdict, cfg Config) Verdict {
	mode := NormalizeLocalMode(cfg.Advanced.Enforcement.LocalMode)
	if mode == ModeBlock || verdict.LocalOriginalAction != "" {
		return verdict
	}
	verdict.LocalOriginalAction = verdict.Action
	verdict.LocalMode = mode
	verdict = ApplyReviewMode(verdict, mode)
	verdict.LocalAction = verdict.Action
	// Keeping local evidence for audit must not turn a later model-only block
	// into local terminal evidence or a user strike.
	verdict.TerminalStrictHit = false
	verdict.TerminalCategoryHit = false
	if verdict.LocalOriginalAction != ActionAllow {
		verdict.Reason = "local " + mode + ": " + verdict.Reason
	}
	return verdict
}

func auxiliaryGuardOrigin(origin SegmentOrigin) bool {
	switch origin {
	case OriginHistory, OriginSystem, OriginDeveloper, OriginInstructions, OriginToolOutput, OriginToolArguments, OriginAttachmentRefs, OriginSessionContext, OriginAttachmentContent:
		return true
	}
	return false
}

// Only a strict operational rule with sufficient evidence in this one segment
// can enforce. Audit scores, signal-only keywords, and incomplete scans cannot.
func highConfidenceAuxiliaryVerdict(verdict Verdict, threshold int) bool {
	if verdict.Action != ActionBlock || !verdict.SensitiveIntent || !verdict.StrictHit || verdict.Score < threshold {
		return false
	}
	for _, match := range verdict.Matched {
		if match.Name == encodedScanIncompleteMatch {
			return false
		}
	}
	for _, match := range verdict.Matched {
		if match.Strict && !match.SignalOnly && match.Weight >= threshold {
			return true
		}
	}
	return false
}
