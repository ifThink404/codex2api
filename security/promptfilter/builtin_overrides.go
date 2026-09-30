package promptfilter

import (
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"strings"
)

// Optional condition fields preserve release defaults for older saved edits.
// An explicit empty list clears a condition; nil retains its release default.
type BuiltinPatternOverride struct {
	Name                         string   `json:"name"`
	Pattern                      string   `json:"pattern"`
	Weight                       int      `json:"weight"`
	Category                     string   `json:"category"`
	Strict                       bool     `json:"strict"`
	SignalOnly                   *bool    `json:"signal_only,omitempty"`
	AllPatterns                  []string `json:"all_patterns"`
	AnyPatterns                  []string `json:"any_patterns"`
	ExcludePatterns              []string `json:"exclude_patterns"`
	AuthorizationExcludePatterns []string `json:"authorization_exclude_patterns"`
	MinMatches                   *int     `json:"min_matches,omitempty"`
}

func BuiltinPatternFields(pattern PatternConfig) BuiltinPatternOverride {
	return BuiltinPatternOverride{
		Name: pattern.Name, Pattern: pattern.Pattern, Weight: pattern.Weight, Category: pattern.Category, Strict: pattern.Strict,
		SignalOnly: &pattern.SignalOnly, MinMatches: &pattern.MinMatches,
		AllPatterns: append([]string{}, pattern.AllPatterns...), AnyPatterns: append([]string{}, pattern.AnyPatterns...),
		ExcludePatterns: append([]string{}, pattern.ExcludePatterns...), AuthorizationExcludePatterns: append([]string{}, pattern.AuthorizationExcludePatterns...),
	}
}

func applyBuiltinPatternOverride(pattern PatternConfig, rule BuiltinPatternOverride) PatternConfig {
	pattern.Pattern, pattern.Weight = rule.Pattern, rule.Weight
	pattern.Category, pattern.Strict = rule.Category, rule.Strict
	if rule.SignalOnly != nil {
		pattern.SignalOnly = *rule.SignalOnly
	}
	if rule.MinMatches != nil {
		pattern.MinMatches = *rule.MinMatches
	}
	for _, pair := range []struct {
		src []string
		dst *[]string
	}{
		{rule.AllPatterns, &pattern.AllPatterns}, {rule.AnyPatterns, &pattern.AnyPatterns},
		{rule.ExcludePatterns, &pattern.ExcludePatterns}, {rule.AuthorizationExcludePatterns, &pattern.AuthorizationExcludePatterns},
	} {
		if pair.src != nil {
			*pair.dst = append([]string(nil), pair.src...)
		}
	}
	return pattern
}

// Compare resolved definitions so legacy snapshots still compare correctly,
// while concurrent edits to any condition invalidate a stale editor.
func BuiltinPatternOverridesEqual(a, b BuiltinPatternOverride) bool {
	if a.Name != b.Name {
		return false
	}
	for _, pattern := range defaultPatternConfigs {
		if pattern.Name == a.Name {
			return reflect.DeepEqual(BuiltinPatternFields(applyBuiltinPatternOverride(pattern, a)), BuiltinPatternFields(applyBuiltinPatternOverride(pattern, b)))
		}
	}
	return reflect.DeepEqual(a, b)
}

func cloneBuiltinPatternOverrides(rules []BuiltinPatternOverride) []BuiltinPatternOverride {
	if rules == nil {
		return nil
	}
	out := make([]BuiltinPatternOverride, len(rules))
	for i, rule := range rules {
		out[i] = rule
		if rule.SignalOnly != nil {
			value := *rule.SignalOnly
			out[i].SignalOnly = &value
		}
		if rule.MinMatches != nil {
			value := *rule.MinMatches
			out[i].MinMatches = &value
		}
		for _, pair := range []struct {
			src []string
			dst *[]string
		}{
			{rule.AllPatterns, &out[i].AllPatterns}, {rule.AnyPatterns, &out[i].AnyPatterns},
			{rule.ExcludePatterns, &out[i].ExcludePatterns}, {rule.AuthorizationExcludePatterns, &out[i].AuthorizationExcludePatterns},
		} {
			if pair.src != nil {
				*pair.dst = append([]string{}, pair.src...)
			}
		}
	}
	return out
}

func ValidateBuiltinPatternOverride(rule BuiltinPatternOverride) error {
	var original *PatternConfig
	for i := range defaultPatternConfigs {
		if defaultPatternConfigs[i].Name == rule.Name {
			original = &defaultPatternConfigs[i]
			break
		}
	}
	if original == nil {
		return fmt.Errorf("内置规则不存在: %s", rule.Name)
	}
	if rule.Weight < 1 || rule.Weight > 1000 {
		return fmt.Errorf("规则权重必须为 1-1000")
	}
	if len(rule.Pattern) > 64*1024 {
		return fmt.Errorf("正则表达式不能超过 64 KiB")
	}
	if len(rule.Category) > 128 {
		return fmt.Errorf("分类不能超过 128 字节")
	}
	effective := applyBuiltinPatternOverride(*original, rule)
	if strings.TrimSpace(rule.Pattern) == "" {
		if len(effective.AllPatterns) == 0 && len(effective.AnyPatterns) == 0 {
			return fmt.Errorf("正则表达式不能为空")
		}
	} else if _, err := regexp.Compile(rule.Pattern); err != nil {
		return fmt.Errorf("正则表达式无效: %w", err)
	}
	if effective.MinMatches < 0 || effective.MinMatches > len(effective.AnyPatterns) {
		return fmt.Errorf("任选条件命中数量必须为 0 到任选条件数量之间的整数")
	}
	totalBytes := len(rule.Pattern)
	for _, list := range []struct {
		name  string
		items []string
	}{
		{"all_patterns", effective.AllPatterns}, {"any_patterns", effective.AnyPatterns},
		{"exclude_patterns", effective.ExcludePatterns}, {"authorization_exclude_patterns", effective.AuthorizationExcludePatterns},
	} {
		if len(list.items) > 128 {
			return fmt.Errorf("%s 不能超过 128 条", list.name)
		}
		for i, expression := range list.items {
			totalBytes += len(expression)
			if strings.TrimSpace(expression) == "" || len(expression) > 64*1024 {
				return fmt.Errorf("%s[%d] 正则不能为空或超过 64 KiB", list.name, i)
			}
			if _, err := regexp.Compile(expression); err != nil {
				return fmt.Errorf("%s[%d] 正则无效: %w", list.name, i, err)
			}
		}
	}
	if totalBytes > 256*1024 {
		return fmt.Errorf("规则正则总长度不能超过 256 KiB")
	}
	return nil
}

func ParseBuiltinPatternOverrides(raw string) ([]BuiltinPatternOverride, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var rules []BuiltinPatternOverride
	if err := json.Unmarshal([]byte(raw), &rules); err != nil {
		return nil, err
	}
	return rules, nil
}

func EffectiveBuiltinPatternConfigs(overrides []BuiltinPatternOverride) []PatternConfig {
	patterns := BuiltinPatternConfigs()
	byName := make(map[string]BuiltinPatternOverride, len(overrides))
	for _, rule := range overrides {
		// Invalid or retired persisted edits must not disable the engine. Their
		// release defaults stay active; explicit saves are validated by the API.
		if ValidateBuiltinPatternOverride(rule) == nil {
			byName[rule.Name] = rule
		}
	}
	for i := range patterns {
		if rule, ok := byName[patterns[i].Name]; ok {
			patterns[i] = applyBuiltinPatternOverride(patterns[i], rule)
		}
	}
	return patterns
}
