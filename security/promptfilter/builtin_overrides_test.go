package promptfilter

import (
	"encoding/json"
	"testing"
)

func TestBuiltinOverridesChangeCachedEngineAndRestore(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Enabled = true
	before, err := engineForConfig(cfg)
	ruleRequireNoError(t, err)
	edit := BuiltinPatternOverride{Name: "prompt_fake_authorization", Pattern: "builtin_override_probe_987654", Weight: 83, Category: "prompt_injection", Strict: true}
	ruleRequireNoError(t, ValidateBuiltinPatternOverride(edit))
	cfg.BuiltinOverrides = []BuiltinPatternOverride{edit}
	after, err := engineForConfig(cfg)
	ruleRequireNoError(t, err)
	ruleRequireNotSame(t, before, after)
	verdict := after.InspectText("builtin_override_probe_987654")
	found := 0
	for _, match := range verdict.Matched {
		if match.Name == edit.Name {
			found++
			ruleRequireEqual(t, edit.Weight, match.Weight)
		}
	}
	ruleRequireEqual(t, 1, found, "the override must replace, not duplicate, the builtin")
	copy := NormalizeConfig(cfg)
	copy.BuiltinOverrides[0].Weight = 1
	ruleRequireEqual(t, 83, cfg.BuiltinOverrides[0].Weight)
	cfg.BuiltinOverrides = nil
	restored, err := engineForConfig(cfg)
	ruleRequireNoError(t, err)
	ruleRequireSame(t, before, restored)
	ruleRequireEmpty(t, restored.InspectText("builtin_override_probe_987654").Matched)
}

func TestBuiltinOverrideCompleteConditionsAndScoring(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Enabled, cfg.Mode = true, ModeBlock
	cfg.Advanced.ContextDiscount.Enabled = false
	var original PatternConfig
	for _, rule := range BuiltinPatternConfigs() {
		if rule.Name == "generic_exploit" {
			original = rule
		} else {
			cfg.DisabledPatterns = append(cfg.DisabledPatterns, rule.Name)
		}
	}
	edit := BuiltinPatternFields(original)
	edit.Pattern, edit.Weight, edit.Category = "primary_probe", 75, "probe"
	edit.AllPatterns = []string{"required_probe"}
	edit.AnyPatterns = []string{"alternative_alpha", "alternative_beta"}
	edit.ExcludePatterns = []string{"excluded_probe"}
	edit.AuthorizationExcludePatterns = []string{"authorized_probe"}
	minimum := 2
	edit.MinMatches = &minimum
	cfg.BuiltinOverrides = []BuiltinPatternOverride{edit}
	text := "primary_probe required_probe alternative_alpha alternative_beta"
	inspect := func(input string) Verdict {
		t.Helper()
		engine, err := engineForConfig(cfg)
		ruleRequireNoError(t, err)
		return engine.InspectText(input)
	}
	for _, input := range []string{
		"required_probe alternative_alpha alternative_beta", "primary_probe alternative_alpha alternative_beta",
		"primary_probe required_probe alternative_alpha", text + " excluded_probe",
	} {
		ruleRequireEmpty(t, inspect(input).Matched, input)
	}
	verdict := inspect(text)
	ruleRequireLen(t, verdict.Matched, 1)
	ruleRequireTrue(t, verdict.Matched[0].SignalOnly)
	ruleRequireEqual(t, ActionAllow, verdict.Action)
	ruleRequireLess(t, verdict.Score, cfg.Threshold)
	auditEngine, err := engineForConfig(cfg)
	ruleRequireNoError(t, err)
	signalOnly := false
	edit.SignalOnly = &signalOnly
	cfg.BuiltinOverrides = []BuiltinPatternOverride{edit}
	executionEngine, err := engineForConfig(cfg)
	ruleRequireNoError(t, err)
	ruleRequireNotSame(t, auditEngine, executionEngine)
	ruleRequireEqual(t, ActionBlock, inspect(text).Action)
	ruleRequireEqual(t, 75, inspect(text).Score)
	cfg.Advanced.Enforcement.AuthorizedPentestAllowed = false
	ruleRequireLen(t, inspect(text+" authorized_probe").Matched, 1)
	cfg.Advanced.Enforcement.AuthorizedPentestAllowed = true
	ruleRequireEmpty(t, inspect(text+" authorized_probe").Matched)
	// Empty arrays clear conditions, including conditional exclusions.
	edit.Pattern = ""
	edit.AnyPatterns, edit.ExcludePatterns, edit.AuthorizationExcludePatterns = []string{}, []string{}, []string{}
	zero := 0
	edit.MinMatches = &zero
	cfg.BuiltinOverrides = []BuiltinPatternOverride{edit}
	ruleRequireEqual(t, ActionBlock, inspect("required_probe excluded_probe authorized_probe").Action)
	// The existing strict-over-signal rule remains visible, not silently changed.
	signalOnly = true
	edit.Strict = true
	cfg.BuiltinOverrides = []BuiltinPatternOverride{edit}
	ruleRequireEqual(t, ActionBlock, inspect("required_probe").Action)
}

func TestBuiltinOverrideLegacyPersistenceAndSnapshotIsolation(t *testing.T) {
	for _, original := range BuiltinPatternConfigs() {
		legacy := BuiltinPatternOverride{Name: original.Name, Pattern: original.Pattern, Weight: original.Weight, Category: original.Category, Strict: original.Strict}
		data, err := json.Marshal([]BuiltinPatternOverride{legacy})
		ruleRequireNoError(t, err)
		parsed, err := ParseBuiltinPatternOverrides(string(data))
		ruleRequireNoError(t, err)
		ruleRequireTrue(t, BuiltinPatternOverridesEqual(BuiltinPatternFields(original), parsed[0]), original.Name)
		for _, effective := range EffectiveBuiltinPatternConfigs(parsed) {
			if effective.Name == original.Name {
				ruleRequireEqual(t, original, effective)
			}
		}
	}
	edit := BuiltinPatternFields(PatternConfig{Name: "generic_exploit", Pattern: "isolated_probe", Weight: 75, AllPatterns: []string{"required_probe"}, SignalOnly: true})
	cfg := NormalizeConfig(Config{BuiltinOverrides: []BuiltinPatternOverride{edit}})
	cfg.BuiltinOverrides[0].AllPatterns[0] = "changed_probe"
	*cfg.BuiltinOverrides[0].SignalOnly = false
	*cfg.BuiltinOverrides[0].MinMatches = 1
	ruleRequireEqual(t, "required_probe", edit.AllPatterns[0])
	ruleRequireTrue(t, *edit.SignalOnly)
	ruleRequireZero(t, *edit.MinMatches)
	ruleRequireFalse(t, BuiltinPatternOverridesEqual(edit, cfg.BuiltinOverrides[0]))
}

func TestBuiltinOverrideRejectsInvalidConditions(t *testing.T) {
	base := BuiltinPatternFields(PatternConfig{Name: "generic_exploit", Pattern: "primary_probe", Weight: 50})
	for _, field := range []string{"all", "any", "exclude", "authorization"} {
		t.Run(field, func(t *testing.T) {
			for _, expression := range []string{"", "["} {
				edit := base
				switch field {
				case "all":
					edit.AllPatterns = []string{expression}
				case "any":
					edit.AnyPatterns = []string{expression}
				case "exclude":
					edit.ExcludePatterns = []string{expression}
				case "authorization":
					edit.AuthorizationExcludePatterns = []string{expression}
				}
				ruleRequireError(t, ValidateBuiltinPatternOverride(edit))
			}
		})
	}
	for _, minimum := range []int{-1, 1} {
		edit := base
		edit.MinMatches = &minimum
		ruleRequireError(t, ValidateBuiltinPatternOverride(edit))
	}
	base.Pattern = ""
	ruleRequireError(t, ValidateBuiltinPatternOverride(base))
}

func TestCustomPatternAuthorizationConditionsValidatedWhileDisabled(t *testing.T) {
	rule := PatternConfig{Name: "conditional_custom", Pattern: "primary_probe_987654", Weight: 75, AuthorizationExcludePatterns: []string{"["}}
	ruleRequireError(t, ValidateCustomPatterns([]PatternConfig{rule}))
	rule.AuthorizationExcludePatterns = []string{"authorized_probe"}
	ruleRequireNoError(t, ValidateCustomPatterns([]PatternConfig{rule}))
}

func TestBuiltinOverridesKeepCompositeConditionsAndDefaults(t *testing.T) {
	for _, original := range BuiltinPatternConfigs() {
		t.Run(original.Name, func(t *testing.T) {
			edit := BuiltinPatternFields(original)
			ruleRequireNoError(t, ValidateBuiltinPatternOverride(edit))
			edit.Weight = 123
			for _, effective := range EffectiveBuiltinPatternConfigs([]BuiltinPatternOverride{edit}) {
				if effective.Name != original.Name {
					continue
				}
				ruleRequireEqual(t, 123, effective.Weight)
				effective.Weight = original.Weight
				ruleRequireEqual(t, original, effective)
			}
		})
	}
	bad := BuiltinPatternOverride{Name: "prompt_fake_authorization", Pattern: "[", Weight: 20}
	ruleRequireError(t, ValidateBuiltinPatternOverride(bad))
	ruleRequireEqual(t, BuiltinPatternConfigs(), EffectiveBuiltinPatternConfigs([]BuiltinPatternOverride{bad}))
	bad.Name = "missing"
	ruleRequireError(t, ValidateBuiltinPatternOverride(bad))
}
