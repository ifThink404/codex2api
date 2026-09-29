package proxy

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/codex2api/security"
)

// fileLogger 单个日志文件实例。文件超过 errorLogMaxBytes 时按大小轮转：
// name.log → name.log.1 → … → name.log.<errorLogBackups>，最旧的一份被丢弃。
type fileLogger struct {
	once   sync.Once
	mu     sync.Mutex
	logger *log.Logger
	file   *os.File
	path   string
	size   int64
}

var (
	badRequestLogger          = &fileLogger{path: "bad_request.log"}           // 400 错误
	serverErrorLogger         = &fileLogger{path: "server_error.log"}          // 5xx 错误
	upstreamClientErrorLogger = &fileLogger{path: "upstream_client_error.log"} // 其余 4xx，包括 422/429
)

const defaultLogDir = "logs"

// Error log rotation defaults: LOG_MAX_SIZE_MB (1-1024) and LOG_MAX_BACKUPS
// (1-20) override them.
const (
	defaultErrorLogMaxMB   = 20
	defaultErrorLogBackups = 3
)

func errorLogMaxBytes() int64 {
	if mb, err := strconv.Atoi(strings.TrimSpace(os.Getenv("LOG_MAX_SIZE_MB"))); err == nil && mb >= 1 && mb <= 1024 {
		return int64(mb) << 20
	}
	return defaultErrorLogMaxMB << 20
}

func errorLogBackups() int {
	if n, err := strconv.Atoi(strings.TrimSpace(os.Getenv("LOG_MAX_BACKUPS"))); err == nil && n >= 1 && n <= 20 {
		return n
	}
	return defaultErrorLogBackups
}
const upstreamErrorLogBodyMaxBytes = 8 * 1024

func upstreamErrorLogBody(body []byte) string {
	if len(strings.TrimSpace(string(body))) > 0 && !json.Valid(body) {
		return fmt.Sprintf("[non-JSON upstream body omitted; %d bytes]", len(body))
	}
	truncated := false
	if len(body) > upstreamErrorLogBodyMaxBytes {
		body = body[:upstreamErrorLogBodyMaxBytes]
		truncated = true
	}
	bodyStr := security.MaskSensitiveData(string(body))
	bodyStr = security.SafeTruncate(bodyStr, 5000)
	if truncated {
		bodyStr += " ... [truncated]"
	}
	return bodyStr
}

func errorLogDir() string {
	if dir := strings.TrimSpace(os.Getenv("LOG_DIR")); dir != "" {
		return dir
	}
	return defaultLogDir
}

func (fl *fileLogger) init() *log.Logger {
	if security.FileLogsDisabled() {
		return nil
	}
	fl.once.Do(func() {
		dir := errorLogDir()
		if err := os.MkdirAll(dir, 0o755); err != nil {
			log.Printf("创建日志目录失败: %v", err)
			return
		}
		f, err := os.OpenFile(filepath.Join(dir, fl.path), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			log.Printf("打开日志文件 %s 失败: %v", fl.path, err)
			return
		}
		fl.file = f
		fl.logger = log.New(f, "", 0)
		if info, err := f.Stat(); err == nil {
			fl.size = info.Size()
		}
	})
	return fl.logger
}

// rotateLocked shifts name.log.N-1 → name.log.N … name.log → name.log.1 and
// reopens an empty name.log. Callers hold fl.mu.
func (fl *fileLogger) rotateLocked() {
	full := filepath.Join(errorLogDir(), fl.path)
	backups := errorLogBackups()
	if fl.file != nil {
		_ = fl.file.Close()
	}
	_ = os.Remove(fmt.Sprintf("%s.%d", full, backups))
	for i := backups - 1; i >= 1; i-- {
		_ = os.Rename(fmt.Sprintf("%s.%d", full, i), fmt.Sprintf("%s.%d", full, i+1))
	}
	_ = os.Rename(full, full+".1")
	f, err := os.OpenFile(full, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		log.Printf("轮转日志文件 %s 失败: %v", fl.path, err)
		fl.file, fl.logger = nil, nil
		return
	}
	fl.file, fl.logger, fl.size = f, log.New(f, "", 0), 0
}

// write appends one entry, rotating first when it would exceed the size cap.
func (fl *fileLogger) write(entry string) {
	fl.mu.Lock()
	defer fl.mu.Unlock()
	if fl.logger == nil {
		return
	}
	if fl.size > 0 && fl.size+int64(len(entry)) > errorLogMaxBytes() {
		fl.rotateLocked()
		if fl.logger == nil {
			return
		}
	}
	fl.logger.Print(entry)
	fl.size += int64(len(entry))
	if !strings.HasSuffix(entry, "\n") {
		fl.size++
	}
}

func (fl *fileLogger) close() {
	fl.mu.Lock()
	defer fl.mu.Unlock()
	if fl.file != nil {
		if err := fl.file.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "关闭日志文件 %s 失败: %v\n", fl.path, err)
		}
	}
}

// writeEntry 写一条错误日志（自动脱敏敏感信息）
func (fl *fileLogger) writeEntry(endpoint string, statusCode int, model string, accountID int64, body []byte) {
	l := fl.init()
	if l == nil {
		return
	}

	// 脱敏日志内容
	safeEndpoint := security.SanitizeLog(endpoint)
	safeModel := security.SanitizeLog(model)
	bodyStr := upstreamErrorLogBody(body)

	ts := time.Now().Format("2006/01/02 15:04:05")
	fl.write(fmt.Sprintf("========== %s ==========\nEndpoint: %s\nStatus: %d\nModel: %s\nAccount: %d\nResponse:\n%s\n",
		ts, safeEndpoint, statusCode, safeModel, accountID, bodyStr))
}

// logUpstreamError 根据状态码分发到对应日志文件
func logUpstreamError(endpoint string, statusCode int, model string, accountID int64, body []byte) {
	switch {
	case statusCode == 400:
		badRequestLogger.writeEntry(endpoint, statusCode, model, accountID, body)
	case statusCode >= 500:
		serverErrorLogger.writeEntry(endpoint, statusCode, model, accountID, body)
	case statusCode > 400:
		// 只写提取后的原因与响应形态，不写可能回显请求输入的原始正文。
		upstreamClientErrorLogger.writeEntry(endpoint, statusCode, model, accountID, upstreamClientErrorLogRecord(body))
	}
}

// CloseErrorLogger 关闭所有错误日志文件（程序退出时调用）
func CloseErrorLogger() {
	badRequestLogger.close()
	serverErrorLogger.close()
	upstreamClientErrorLogger.close()
}
