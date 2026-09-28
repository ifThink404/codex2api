// Package logagent 是与具体功能解耦的日志分析 Agent：调用方（运维错误页、各传输
// 插件的抓包/日志页……）通过 Source 提供证据记录，logagent 负责把记录压成有界、
// 脱敏的上下文，调用 LLM 并把输出解析成结构化结论。
//
// 本包只依赖 LLM 这一个小接口，不引用 proxy / admin / 插件代码；号池实现放在 admin 包。
package logagent

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"sync"
	"time"
)

// LLM 是分析所需的最小模型接口：给定模型、系统指令与输入，返回模型输出文本。
type LLM interface {
	Respond(ctx context.Context, model, instructions, input string) (string, error)
}

// Usage 是一次模型调用的 token 用量；实现方拿不到时全为 0。
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

// UsageLLM 是可选扩展：实现了它的 LLM 会额外上报 token 用量，Analyze 会优先调用。
type UsageLLM interface {
	LLM
	RespondWithUsage(ctx context.Context, model, instructions, input string) (string, Usage, error)
}

// Record 是一条证据记录。ID 在一次分析内唯一，模型引用证据时只能引用这些 ID；
// 其余文本字段都视为不可信数据，进入上下文前一律脱敏。
type Record struct {
	ID        string            `json:"id"`
	Kind      string            `json:"kind,omitempty"`
	Time      time.Time         `json:"time"`
	Status    int               `json:"status,omitempty"`
	ErrorKind string            `json:"error_kind,omitempty"`
	Message   string            `json:"message,omitempty"`
	Fields    map[string]string `json:"fields,omitempty"`
	// Body 是可选的大段载荷（抓包响应体、上游错误原文），预算紧张时最先被裁掉。
	Body string `json:"body,omitempty"`
	// Signature 可选：同签名的记录会合并成一组；为空时按状态码/错误类型/归一化消息/
	// endpoint/model 自动计算。
	Signature string `json:"signature,omitempty"`
}

// Query 是交给 Source 的检索条件：Refs 为记录引用（请求 ID、抓包 ID 等，含义由 Source
// 自定），Filters 为页面上的筛选条件。两者可同时为空，由 Source 决定默认行为。
type Query struct {
	Refs    []string          `json:"refs,omitempty"`
	Filters map[string]string `json:"filters,omitempty"`
	Start   time.Time         `json:"start"`
	End     time.Time         `json:"end"`
	Limit   int               `json:"limit"`
}

// Source 为某一类日志提供证据记录。插件在自己的包里实现并注册，logagent 不反向依赖。
type Source interface {
	Name() string
	Fetch(ctx context.Context, q Query) ([]Record, error)
}

// SourceFunc 把一个函数包装成 Source，方便插件不定义新类型就注册。
func SourceFunc(name string, fetch func(ctx context.Context, q Query) ([]Record, error)) Source {
	return funcSource{name: name, fetch: fetch}
}

type funcSource struct {
	name  string
	fetch func(ctx context.Context, q Query) ([]Record, error)
}

func (s funcSource) Name() string { return s.name }
func (s funcSource) Fetch(ctx context.Context, q Query) ([]Record, error) {
	return s.fetch(ctx, q)
}

// sourceNamePattern 限制 Source 名：它会拼进 usage_logs.internal_reason
// （"log_agent:" + 名称，列宽 64），也会出现在 URL 与持久化记录里。
var sourceNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_.:-]{0,47}$`)

// ValidSourceName 报告名称是否可用作 Source 名。
func ValidSourceName(name string) bool {
	return sourceNamePattern.MatchString(name)
}

var (
	ErrInvalidSourceName = errors.New("logagent: invalid source name")
	ErrDuplicateSource   = errors.New("logagent: source already registered")
)

// Registry 是 Source 注册表，并发安全。
type Registry struct {
	mu      sync.RWMutex
	sources map[string]Source
}

func NewRegistry() *Registry {
	return &Registry{sources: make(map[string]Source)}
}

// Register 注册一个 Source；名称非法或重复时报错。
func (r *Registry) Register(src Source) error {
	if src == nil || !ValidSourceName(src.Name()) {
		return ErrInvalidSourceName
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.sources[src.Name()]; exists {
		return fmt.Errorf("%w: %s", ErrDuplicateSource, src.Name())
	}
	r.sources[src.Name()] = src
	return nil
}

func (r *Registry) Lookup(name string) (Source, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	src, ok := r.sources[name]
	return src, ok
}

// Names 返回已注册的 Source 名，按字母序。
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.sources))
	for name := range r.sources {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Default 是进程级注册表，插件在初始化时调用 Register 挂入自己的 Source。
var Default = NewRegistry()

// Register 向 Default 注册 Source。
func Register(src Source) error {
	return Default.Register(src)
}
