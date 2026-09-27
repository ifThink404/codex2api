package database

func normalizeSessionFailoverSelection(input *SessionFailoverSelection) *SessionFailoverSelection {
	if input == nil {
		return nil
	}
	result := *input
	result.MatchMode = serviceErrorString(result.MatchMode, 64)
	result.RequiredGroupIDs = append([]int64{}, input.RequiredGroupIDs[:min(len(input.RequiredGroupIDs), 32)]...)
	labels := func(values []string) []string {
		out := make([]string, 0, min(len(values), 32))
		for _, value := range values[:min(len(values), 32)] {
			out = append(out, serviceErrorString(value, 128))
		}
		return out
	}
	result.RequiredTags = labels(input.RequiredTags)
	result.RejectionCounts = make(map[string]int)
	for reason, count := range input.RejectionCounts {
		if len(result.RejectionCounts) >= 33 {
			break
		}
		result.RejectionCounts[serviceErrorString(reason, 64)] = count
	}
	result.Candidates = append([]SessionFailoverCandidate(nil), input.Candidates[:min(len(input.Candidates), 20)]...)
	for i := range result.Candidates {
		candidate := &result.Candidates[i]
		candidate.Reason = serviceErrorString(candidate.Reason, 64)
		candidate.GroupIDs = append([]int64{}, candidate.GroupIDs[:min(len(candidate.GroupIDs), 32)]...)
		candidate.Tags = labels(candidate.Tags)
		if candidate.IdentityFailure != nil {
			detail := *candidate.IdentityFailure
			detail.Stage = serviceErrorString(detail.Stage, 64)
			detail.Status = serviceErrorString(detail.Status, 64)
			detail.Code = serviceErrorString(detail.Code, 96)
			if detail.HTTPStatus < 400 || detail.HTTPStatus > 599 {
				detail.HTTPStatus = 0
			}
			candidate.IdentityFailure = &detail
		}
	}
	return &result
}
