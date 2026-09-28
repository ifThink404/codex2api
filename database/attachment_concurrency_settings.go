package database

const (
	DefaultBPSAttachmentRequestConcurrency  = 15
	MaxBPSAttachmentRequestConcurrency      = 64
	DefaultBPSAttachmentInstanceConcurrency = 64
	MaxBPSAttachmentInstanceConcurrency     = 1024
	DefaultResinAccountMaxConns             = 15
	MaxResinAccountMaxConns                 = 1024
)

func NormalizeBPSAttachmentRequestConcurrency(value int) int {
	if value < 1 || value > MaxBPSAttachmentRequestConcurrency {
		return DefaultBPSAttachmentRequestConcurrency
	}
	return value
}

func NormalizeBPSAttachmentInstanceConcurrency(value int) int {
	if value < 1 || value > MaxBPSAttachmentInstanceConcurrency {
		return DefaultBPSAttachmentInstanceConcurrency
	}
	return value
}

func NormalizeResinAccountMaxConns(value int) int {
	if value < 1 || value > MaxResinAccountMaxConns {
		return DefaultResinAccountMaxConns
	}
	return value
}
