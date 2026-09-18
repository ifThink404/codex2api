package database

import "strings"

// usage_logs 的 turn-state 三列：每条日志记录本次胜出尝试的 X-Codex-Turn-State 情况。
//
//	turn_state_length   INT NULL       上游首次返回的真实 token 的字符数。
//	                                   0   = 已检查上游响应但它没给
//	                                   NULL= 未记录（历史行 / 拿到上游响应前失败 / 非官方路径）
//	turn_state_echo     VARCHAR(16)    入站回带分类，'' = 未记录
//	turn_state_stripped BOOLEAN        本次入站值是否被网关剥离
//
// 长度是账号级的「降智桶」标记：线上实测同一时刻健康号 292 字符、其余号 312 字符，
// 与单次请求成败无关。按账号看这一列的分布就是「降智账号」的直接读数。

// UsageLogTurnStateEchoClasses 是 turn_state_echo 的合法取值。分类由网关自己产生，
// 但落库前仍按白名单过滤：这列会被当枚举筛选，混进任何自由文本都会让筛选项列表失真。
var UsageLogTurnStateEchoClasses = []string{"none", "same", "cross", "unknown", "substitute"}

// usageLogTurnStateLengthMax 是 PostgreSQL INT 的上限。真实 token 只有三百来字符，
// 这里只是不让畸形输入撑爆列：超限按上限截，而不是让整批 INSERT 回滚。
const usageLogTurnStateLengthMax = 2147483647

// normalizeUsageLogTurnStateEcho 把未知分类归为「未记录」而不是截断存下：
// 截断出来的半截词会变成一个永远筛不到东西的假选项。
func normalizeUsageLogTurnStateEcho(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	for _, allowed := range UsageLogTurnStateEchoClasses {
		if value == allowed {
			return value
		}
	}
	return ""
}

// normalizeUsageLogTurnStateLength 保留 nil（未记录）与 0（检查过但没有）的区别，
// 只把负数与超列宽的值夹到合法区间。
func normalizeUsageLogTurnStateLength(value *int) *int {
	if value == nil {
		return nil
	}
	length := *value
	if length < 0 {
		length = 0
	}
	if length > usageLogTurnStateLengthMax {
		length = usageLogTurnStateLengthMax
	}
	return &length
}

// nullableUsageLogTurnStateLength 把指针转成驱动参数：nil 必须写成 SQL NULL，
// 不能退化成 0。
func nullableUsageLogTurnStateLength(value *int) any {
	if value == nil {
		return nil
	}
	return *value
}
