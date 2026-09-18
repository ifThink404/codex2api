package proxy

import (
	"sync"

	"github.com/gin-gonic/gin"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
)

// 每条用量日志记录本次**胜出尝试**的 turn-state 情况，三件事分开记：
//
//   - 上游真实 X-Codex-Turn-State 的字符数。线上实测同一时刻健康号 292 字符、其余
//     九个号 312 字符——长度是账号级的「降智桶」标记，不是单次请求的成败信号。
//     按账号看这一列就是「降智账号」的直接读数。
//   - 入站回带的分类（客户端带回来的是谁铸的）。
//   - 本次入站值是否被网关剥离。
//
// 采集分两条时间线，因此也分两个槽位：
//
//	入站分类在出站前只发生一次，failover 换号不改变「客户端带了什么」这个事实，
//	所以第一次判定优先（noteUsageTurnStateEcho 不覆盖）。
//	上游真实 token 则是每次尝试各看各的：attempt 1 拿到了 292、attempt 2 连响应都
//	没拿到，这条日志必须记 NULL 而不是 292，否则换号后旧账号的读数会盖住新账号的。
//	所以长度槽位按尝试隔离：beginUsageTurnStateAttempt 换一个新槽位，旧流的迟到事件
//	写进它自己捕获的旧槽位，对当前尝试不可见。

const (
	usageTurnStateEchoNone       = "none"
	usageTurnStateEchoSame       = "same"
	usageTurnStateEchoCross      = "cross"
	usageTurnStateEchoUnknown    = "unknown"
	usageTurnStateEchoSubstitute = "substitute"
)

// usageTurnStateContextKey 存整条记录（回带分类 + 当前尝试槽位）。
const usageTurnStateContextKey = "usage_turn_state"

// usageTurnStateAttempt 是一次尝试的长度槽位。观察点可能在上游排水协程里触发，
// 所以自带锁；调用方持有的是指针，不再回头查上下文。
type usageTurnStateAttempt struct {
	mu      sync.Mutex
	checked bool
	length  int
}

// mark 记「本次尝试检查过上游的 turn-state 载体」。length=0 表示检查过但上游没给，
// 与「没记录」（槽位 checked=false）是两回事。
//
// 同一次尝试里真实 token 可能经两个载体汇报（HTTP 响应头 + response.metadata 事件）。
// 已经记到真实长度之后，后到的「检查过但没有」不能把它覆盖回 0：WS 传输上只有事件
// 载体、HTTP 上两者同值，出现分歧时长度不为零的那个才是上游真的给过的证据。
func (a *usageTurnStateAttempt) mark(length int) {
	if a == nil {
		return
	}
	if length < 0 {
		length = 0
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.checked {
		a.checked, a.length = true, length
		return
	}
	if length > 0 && a.length == 0 {
		a.length = length
	}
}

func (a *usageTurnStateAttempt) snapshot() (bool, int) {
	if a == nil {
		return false, 0
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.checked, a.length
}

type usageTurnStateRecord struct {
	mu       sync.Mutex
	attempt  *usageTurnStateAttempt
	echoSet  bool
	echo     string
	stripped bool
}

func (r *usageTurnStateRecord) beginAttempt() *usageTurnStateAttempt {
	slot := &usageTurnStateAttempt{}
	r.mu.Lock()
	r.attempt = slot
	r.mu.Unlock()
	return slot
}

func (r *usageTurnStateRecord) currentAttempt() *usageTurnStateAttempt {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.attempt == nil {
		r.attempt = &usageTurnStateAttempt{}
	}
	return r.attempt
}

func (r *usageTurnStateRecord) noteEcho(class string, stripped bool) {
	if class == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.echoSet {
		return
	}
	r.echoSet, r.echo, r.stripped = true, class, stripped
}

func (r *usageTurnStateRecord) echoSnapshot() (bool, string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.echoSet, r.echo, r.stripped
}

func (r *usageTurnStateRecord) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.attempt = nil
	r.echoSet, r.echo, r.stripped = false, "", false
}

// usageTurnStateRecordFor 取（必要时创建）本请求的记录。只在请求协程里调用。
func usageTurnStateRecordFor(c *gin.Context) *usageTurnStateRecord {
	if c == nil {
		return nil
	}
	if existing := usageTurnStateRecordIfPresent(c); existing != nil {
		return existing
	}
	record := &usageTurnStateRecord{}
	c.Set(usageTurnStateContextKey, record)
	return record
}

func usageTurnStateRecordIfPresent(c *gin.Context) *usageTurnStateRecord {
	if c == nil {
		return nil
	}
	if value, exists := c.Get(usageTurnStateContextKey); exists {
		if record, ok := value.(*usageTurnStateRecord); ok {
			return record
		}
	}
	return nil
}

// beginUsageTurnStateAttempt 在每次尝试开始时换一个新的长度槽位，并把它返回给
// 调用方捕获。返回值给那些在流里异步观察事件的闭包用：它们写自己那一次的槽位，
// 换号之后不会污染新尝试。
func beginUsageTurnStateAttempt(c *gin.Context) *usageTurnStateAttempt {
	record := usageTurnStateRecordFor(c)
	if record == nil {
		// 没有上下文（内部调用 / 测试）时给一个孤立槽位，让调用方的写入无害。
		return &usageTurnStateAttempt{}
	}
	return record.beginAttempt()
}

// beginUsageTurnStateTurn 清空整条记录。WebSocket 上一条 gin.Context 要服务同一
// 连接的多个轮次，每轮的入站回带各算各的，不清就会把第一轮的分类粘到后面所有轮。
func beginUsageTurnStateTurn(c *gin.Context) {
	if record := usageTurnStateRecordIfPresent(c); record != nil {
		record.reset()
	}
}

// currentUsageTurnStateAttempt 取当前尝试槽位，供与请求协程同步的观察点使用。
func currentUsageTurnStateAttempt(c *gin.Context) *usageTurnStateAttempt {
	record := usageTurnStateRecordFor(c)
	if record == nil {
		return nil
	}
	return record.currentAttempt()
}

// markUsageTurnStateChecked 记录「本次尝试检查过上游的 turn-state，长度是 length」。
// length=0 = 检查过但上游没给。
func markUsageTurnStateChecked(c *gin.Context, length int) {
	currentUsageTurnStateAttempt(c).mark(length)
}

// noteUsageTurnStateEcho 记录入站回带分类；首个判定优先。
func noteUsageTurnStateEcho(c *gin.Context, class string, stripped bool) {
	if record := usageTurnStateRecordFor(c); record != nil {
		record.noteEcho(class, stripped)
	}
}

// usageTurnStateAppliesToAccount：turn-state 只对官方 Codex OAuth 账号有意义。
// relay / Grok / Antigravity / Claude 账号服务的客户端可以是任何东西，它们回带的
// blob 不具备同一套语义；把 session_guards_policy 设成 off 的账号同理——运营者已经
// 宣布那号上的客户端不按 Codex 契约走。判据与窗口号同源（windowNumberAppliesToAccount），
// 但不复用 turnStateVaultAppliesTo：托管还受 CodexTurnStateVaultEnabled 开关控制，
// 而记录长度与那个开关无关。
func usageTurnStateAppliesToAccount(account *auth.Account) bool {
	return sessionGuardsActiveFor(account)
}

// populateUsageTurnState 在日志落库前补上三列。放在 logUsageForRequest 的统一填充
// 链里，而不是逐个 UsageLogInput 字面量里手写：漏掉一条就等于该路径的 turn-state
// 永久缺失，而这条信息只有请求侧拿得到。
//
// 重试行（IsRetryAttempt）照记：那一次尝试观察到什么就记什么，这正是「哪次换号
// 之后拿不到 turn-state 了」的逐次读数。
//
// 挂在 Handler 上是因为「是不是官方账号」只有号池答得出：UsageLogInput 只带
// AccountID，日志侧要回查一次（与 populateUsageWindowNumber 同源的查法）。
func (h *Handler) populateUsageTurnState(c *gin.Context, input *database.UsageLogInput) {
	if c == nil || input == nil {
		return
	}
	// 调用方已经显式给过就不覆盖（目前没有这样的落库点，留作兜底）。
	if input.TurnStateLength != nil || input.TurnStateEcho != "" || input.TurnStateStripped {
		return
	}
	// 账号无从判定时一律不记：宁可缺一个展示字段，也不把非官方路径的值混进来。
	if h == nil || h.store == nil || input.AccountID <= 0 {
		return
	}
	if !usageTurnStateAppliesToAccount(h.store.FindByID(input.AccountID)) {
		return
	}
	record := usageTurnStateRecordIfPresent(c)
	if record == nil {
		return
	}
	if echoSet, echo, stripped := record.echoSnapshot(); echoSet {
		input.TurnStateEcho = echo
		input.TurnStateStripped = stripped
	}
	record.mu.Lock()
	attempt := record.attempt
	record.mu.Unlock()
	if checked, length := attempt.snapshot(); checked {
		value := length
		input.TurnStateLength = &value
	}
}
