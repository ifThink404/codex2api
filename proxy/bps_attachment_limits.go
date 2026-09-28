package proxy

import (
	"os"
	"strconv"
	"strings"

	"github.com/codex2api/database"
)

// Deployment limits are read at use time; values outside the documented bounds
// use conservative defaults. No per-account throttling or retry policy lives here.
func bpsAttachmentInt(name string, fallback, low, high int) int {
	v, err := strconv.Atoi(strings.TrimSpace(os.Getenv(name)))
	if err != nil || v < low || v > high {
		return fallback
	}
	return v
}

func bpsLocalAttachmentLimit() int {
	return bpsAttachmentInt("CODEX_BPS_ATTACHMENT_CACHE_ENTRIES", bpsAttachmentCacheLimit, 1, 65536)
}

func bpsLocalAttachmentBytes() int64 {
	return int64(bpsAttachmentInt("CODEX_BPS_ATTACHMENT_CACHE_MIB", 64, 1, 1024)) << 20
}

func bpsRequestUploadLimit() int {
	if settings, ok := runtimeSettings.Load().(RuntimeSettings); ok {
		return database.NormalizeBPSAttachmentRequestConcurrency(settings.BPSAttachmentRequestConcurrency)
	}
	return database.DefaultBPSAttachmentRequestConcurrency
}

func bpsInstanceUploadLimit() int {
	if settings, ok := runtimeSettings.Load().(RuntimeSettings); ok {
		return database.NormalizeBPSAttachmentInstanceConcurrency(settings.BPSAttachmentInstanceConcurrency)
	}
	return database.DefaultBPSAttachmentInstanceConcurrency
}

func bpsAccountUploadLimit() int {
	if settings, ok := runtimeSettings.Load().(RuntimeSettings); ok {
		return database.NormalizeBPSAttachmentAccountConcurrency(settings.BPSAttachmentAccountConcurrency)
	}
	return database.DefaultBPSAttachmentAccountConcurrency
}

func bpsUploadBufferLimit() int64 {
	return int64(bpsAttachmentInt("CODEX_BPS_ATTACHMENT_BUFFER_MIB", 512, 64, 16384)) << 20
}
