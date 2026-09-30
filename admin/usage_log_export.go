package admin

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/codex2api/database"
	"github.com/codex2api/security"
	"github.com/gin-gonic/gin"
)

// usageLogExportMaxRows bounds one download. The file is streamed to a temp
// file first, so memory stays flat; the cap keeps disk use and query time
// bounded. A truncated export reports complete=false.
const usageLogExportMaxRows = 200000

var errUsageLogExportLimit = errors.New("usage log export row limit reached")

var usageExportSecretPattern = regexp.MustCompile(`(?i)(bearer\s+|sk-)[A-Za-z0-9._~+/=-]+|(access[_-]?token|refresh[_-]?token|id[_-]?token|api[_-]?key|password|secret)["']?\s*[:=]\s*["']?[^"'\s&,]+`)

// usageLogExportFilterKeys are the usage-log list query parameters echoed
// (redacted) into the export header so a file documents its own selection.
var usageLogExportFilterKeys = []string{"start", "end", "q", "model", "endpoint", "api_key_id", "account_id", "fast", "ultra", "upstream_model_mismatch", "stream", "compact", "has_compaction_history", "channel", "status", "status_code", "error_only", "error_kind", "retry", "via_websocket", "include_canceled", "email", "request_id", "upstream_request_id", "turn_state", "turn_state_length", "turn_state_echo", "turn_state_stripped"}

// ExportUsageLogs downloads usage logs as JSON after an explicit confirmation.
// scope=filtered reuses the usage-log list filters (start/end required);
// scope=all exports every retained row, including canceled requests. Rows are
// the same UsageLog records the list API returns, with secret-looking strings
// redacted once more.
func (h *Handler) ExportUsageLogs(c *gin.Context) {
	if c.Query("confirmed") != "true" {
		writeError(c, http.StatusBadRequest, "请先确认下载使用日志")
		return
	}
	scope := c.Query("scope")
	generatedAt := time.Now().UTC()
	var filter database.UsageLogFilter
	filters := make(map[string]string)
	switch scope {
	case "all":
		filter = database.UsageLogFilter{Start: time.Unix(0, 0).UTC(), End: generatedAt, IncludeCanceled: true}
	case "filtered":
		if raw := strings.ToLower(strings.TrimSpace(c.Query("channel"))); raw != "" && raw != "all" && parseUsageChannel(c) == "" {
			writeError(c, http.StatusBadRequest, "无效的渠道筛选")
			return
		}
		start, startErr := time.Parse(time.RFC3339, c.Query("start"))
		end, endErr := time.Parse(time.RFC3339, c.Query("end"))
		if startErr != nil || endErr != nil || end.Before(start) {
			writeError(c, http.StatusBadRequest, "请提供有效的 start/end 时间范围（RFC3339，开始不晚于结束）")
			return
		}
		parsed, ok := parseUsageLogsFilter(c, start, end)
		if !ok {
			return
		}
		filter = parsed
		for _, key := range usageLogExportFilterKeys {
			if value := c.Query(key); value != "" {
				filters[key] = redactUsageExportText(value)
			}
		}
	default:
		writeError(c, http.StatusBadRequest, "scope 必须为 filtered 或 all")
		return
	}
	if !h.usageLogExportBusy.CompareAndSwap(false, true) {
		writeError(c, http.StatusConflict, "已有使用日志正在导出，请等待完成后重试")
		return
	}
	defer h.usageLogExportBusy.Store(false)
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Minute)
	defer cancel()
	file, err := os.CreateTemp("", "codex2api-usage-export-*.json")
	if err != nil {
		writeInternalError(c, err)
		return
	}
	defer func() {
		_ = file.Close()
		_ = os.Remove(file.Name())
	}()
	writer := bufio.NewWriterSize(file, 64*1024)
	err = h.writeUsageLogExport(ctx, writer, scope, filters, filter, generatedAt, usageLogExportMaxRows)
	if err == nil {
		err = writer.Flush()
	}
	if err != nil {
		writeInternalError(c, err)
		return
	}
	info, err := file.Stat()
	if err != nil {
		writeInternalError(c, err)
		return
	}
	if _, err = file.Seek(0, io.SeekStart); err != nil {
		writeInternalError(c, err)
		return
	}
	filename := fmt.Sprintf("usage-logs-%s-%s.json", scope, generatedAt.Format("20060102-150405"))
	c.Header("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
	c.Header("Cache-Control", "no-store")
	c.Header("X-Content-Type-Options", "nosniff")
	c.DataFromReader(http.StatusOK, info.Size(), "application/json; charset=utf-8", file, nil)
}

func (h *Handler) writeUsageLogExport(ctx context.Context, writer io.Writer, scope string, filters map[string]string, filter database.UsageLogFilter, generatedAt time.Time, maxRows int64) error {
	metadata, err := json.Marshal(map[string]any{
		"version": 1, "scope": scope, "generated_at": generatedAt, "filters": filters, "max_rows": maxRows,
	})
	if err != nil {
		return err
	}
	if _, err = fmt.Fprintf(writer, "%s,\"logs\":[\n", metadata[:len(metadata)-1]); err != nil {
		return err
	}
	count := int64(0)
	encoder := json.NewEncoder(writer)
	err = h.db.WalkUsageLogsByFilter(ctx, filter, func(entry *database.UsageLog) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if count >= maxRows {
			return errUsageLogExportLimit
		}
		payload, err := json.Marshal(entry)
		if err != nil {
			return err
		}
		var record any
		decoder := json.NewDecoder(strings.NewReader(string(payload)))
		decoder.UseNumber()
		if err := decoder.Decode(&record); err != nil {
			return err
		}
		if count > 0 {
			if _, err := io.WriteString(writer, ","); err != nil {
				return err
			}
		}
		if err := encoder.Encode(sanitizeUsageExportValue(record, 0)); err != nil {
			return err
		}
		count++
		return nil
	})
	complete := true
	if errors.Is(err, errUsageLogExportLimit) {
		complete, err = false, nil
	}
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(writer, "],\"total\":%d,\"complete\":%t}\n", count, complete)
	return err
}

func redactUsageExportText(value string) string {
	return usageExportSecretPattern.ReplaceAllString(security.MaskURLCredentials(value), "[REDACTED]")
}

func sanitizeUsageExportValue(value any, depth int) any {
	if depth > 32 {
		return "[omitted: nesting limit]"
	}
	switch typed := value.(type) {
	case map[string]any:
		for key, item := range typed {
			leaf := key[strings.LastIndex(key, ".")+1:]
			normalized := strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(leaf, "-", ""), "_", ""))
			switch normalized {
			case "authorization", "proxyauthorization", "cookie", "setcookie", "apikey", "xapikey", "xadminkey", "accesstoken", "refreshtoken", "idtoken", "clientsecret", "password", "secret", "token", "credentials", "requestbody", "responsebody", "prompt", "instructions", "input", "output", "encryptedcontent":
				delete(typed, key)
				continue
			}
			typed[key] = sanitizeUsageExportValue(item, depth+1)
		}
	case []any:
		for index, item := range typed {
			typed[index] = sanitizeUsageExportValue(item, depth+1)
		}
	case string:
		return redactUsageExportText(typed)
	}
	return value
}
