package proxy

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/codex2api/database"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
)

type codexTurnIdentityInput struct {
	Turn    bool
	Root    bool
	Parent  bool
	History bool
	Sources []string
}

type codexTurnIdentityPlan struct {
	claims     []database.CodexIdentityAliasClaim
	epochs     map[string]database.CodexIdentityEpoch
	references map[string]database.CodexIdentityEpoch
}

func codexAccountTurnIdentityInputs(headers http.Header, body []byte) map[string]codexTurnIdentityInput {
	inputs := make(map[string]codexTurnIdentityInput)
	metadata := gjson.GetBytes(body, "client_metadata")
	for index, source := range []gjson.Result{metadata, diagnosticMetadataObject(metadata.Get("x-codex-turn-metadata")), gjson.Parse(headers.Get(codexTurnMetadataHeader))} {
		for _, field := range []string{"turn_id", "root_turn_id", "parent_turn_id"} {
			value := source.Get(field)
			original := strings.ToLower(strings.TrimSpace(value.String()))
			if value.Type != gjson.String || original == "" {
				continue
			}
			if parsed, err := uuid.Parse(original); err == nil {
				original = parsed.String()
			}
			input := inputs[original]
			input.Turn = input.Turn || field == "turn_id"
			input.Root = input.Root || field == "root_turn_id"
			input.Parent = input.Parent || field == "parent_turn_id"
			location := []string{"client_metadata", "client_metadata.x-codex-turn-metadata", "headers.X-Codex-Turn-Metadata"}[index]
			input.Sources = append(input.Sources, location+"."+field)
			inputs[original] = input
		}
	}
	for index, item := range gjson.GetBytes(body, "input").Array() {
		metadata, _ := historyItemMetadata(item)
		value := gjson.ParseBytes(metadata["turn_id"])
		if value.Type != gjson.String || strings.TrimSpace(value.String()) == "" {
			continue
		}
		original := strings.TrimSpace(value.String())
		if id, err := uuid.Parse(original); err == nil {
			original = id.String()
		}
		input := inputs[original]
		input.History = true
		input.Sources = append(input.Sources, fmt.Sprintf("input[%d].internal_chat_message_metadata_passthrough.turn_id", index))
		inputs[original] = input
	}
	return inputs
}

func (fingerprint *CodexFingerprint) prepareAccountTurnIdentity(ctx context.Context, store CodexIdentityStore, mapping *codexAccountIdentity, rootKey string, currentEpoch database.CodexIdentityEpoch, diagnostic *codexAccountIdentityDiagnostic) (*codexTurnIdentityPlan, error) {
	plan := &codexTurnIdentityPlan{epochs: make(map[string]database.CodexIdentityEpoch), references: make(map[string]database.CodexIdentityEpoch)}
	controls := 0
	for _, input := range fingerprint.accountTurnIdentityInputs {
		if input.Turn || input.Root || input.Parent {
			controls++
		}
	}
	if controls > 32 || len(fingerprint.accountTurnIdentityInputs) > 4096 {
		return nil, codexAccountIdentityError("出站轮次身份数量无效，请检查客户端元数据。")
	}
	ordered := make([]string, 0, len(fingerprint.accountTurnIdentityInputs))
	for original := range fingerprint.accountTurnIdentityInputs {
		ordered = append(ordered, original)
	}
	sort.Strings(ordered)
	mapping.turnAliases = make(map[string]string, len(ordered))
	for _, original := range ordered {
		input := fingerprint.accountTurnIdentityInputs[original]
		if mapping.preserveRoot {
			diagnostic.PreservedIDs = append(diagnostic.PreservedIDs, original)
			continue
		}
		parsed, err := uuid.Parse(original)
		historyOnlyV4 := err == nil && parsed.Version() == 4 && input.History && !input.Turn && !input.Root && !input.Parent
		if err != nil || (parsed.Version() != 7 && parsed.Version() != 4) || parsed.Variant() != uuid.RFC4122 || historyOnlyV4 {
			// Older item history can use a local counter/string rather than the
			// current transport UUID. Keep that optional historical grouping
			// usable without weakening the transport metadata validation.
			if input.History && !input.Turn && !input.Root && !input.Parent && len(original) <= 256 {
				key := codexIdentityDigest("history-turn-v1", rootKey, original)
				outbound, mapErr := store.ResolveCodexIdentityUUIDv7(ctx, key, mapping.digest("history-turn:"+rootKey, original))
				if mapErr != nil {
					return nil, codexAccountIdentityError("历史轮次映射暂时不可用，请重试。")
				}
				mapping.turnAliases[original] = outbound
				diagnostic.Changes = append(diagnostic.Changes, mapping.identityChange(original, outbound, "input[].internal_chat_message_metadata_passthrough.turn_id"))
				plan.claims = append(plan.claims, database.CodexIdentityAliasClaim{AliasKey: codexIdentityDigest("codex-account-alias-v1", outbound), SourceKey: key})
				continue
			}
			failure := &codexInvalidTurnIdentityDiagnostic{Stage: "normalized_pre_mapping", Sources: append([]string(nil), input.Sources...), Reason: "invalid_uuid", Expected: "UUIDv4 or UUIDv7/RFC4122", ValueHash: hashRiskIdentity(original), ValueLength: len(original)}
			description := "不是有效 UUID"
			if err == nil {
				failure.UUIDVersion, failure.UUIDVariant = int(parsed.Version()), parsed.Variant().String()
				failure.Reason, description = "unsupported_uuid_version", fmt.Sprintf("为 UUIDv%d", parsed.Version())
				if parsed.Variant() != uuid.RFC4122 {
					failure.Reason, description = "unsupported_uuid_variant", "UUID 变体不符合 RFC4122"
				}
			}
			diagnostic.InvalidTurnIdentity = failure
			source := strings.Join(input.Sources, ", ")
			if source == "" {
				source = "turn_id/root_turn_id"
			}
			return nil, codexAccountIdentityError(fmt.Sprintf("账号级出站轮次映射需要有效的 UUIDv4 或 UUIDv7；%s %s，请检查轮次元数据。", source, description))
		}
		if len(mapping.secret) != 32 {
			return nil, codexAccountIdentityError("出站轮次映射密钥不可用，请核实会话身份策略。")
		}
		original = parsed.String()
		identityKey := codexIdentityDigest("codex-account-turn-epoch-v1", mapping.owner, mapping.account, original)
		referenceKey := codexIdentityDigest("codex-account-turn-reference-v1", rootKey, original)
		turnEpoch, found, bound, err := store.ReadCodexIdentityReference(ctx, referenceKey, identityKey)
		if err != nil {
			return nil, codexAccountIdentityError("暂时无法核实出站轮次映射，请稍后重试。")
		}
		if !found || !input.Parent && !bound && turnEpoch.RootKey == currentEpoch.RootKey && turnEpoch.Generation < currentEpoch.Generation {
			turnEpoch = currentEpoch
		}
		turnMapping := *mapping
		turnMapping.epoch = turnEpoch.Segment
		turnMapping.mode = "account-suffix-v1"
		if turnEpoch.MappingVersion == database.CodexIdentityMappingUUIDv7 || parsed.Version() == 4 {
			// sever accepts SDK UUIDv4 turns, but always sends a persistent,
			// owner/account/epoch-scoped UUIDv7, including on legacy policies.
			turnMapping.mode = database.CodexIdentityMappingUUIDv7
		}
		outbound, err := turnMapping.mapUUID(ctx, store, "turn", original)
		if err != nil {
			return nil, err
		}
		mapping.turnAliases[original] = outbound
		fields := make([]string, 0, 2)
		if input.Turn {
			fields = append(fields, "turn_id")
		}
		if input.Root {
			fields = append(fields, "root_turn_id")
		}
		if input.Parent {
			fields = append(fields, "parent_turn_id")
		}
		if input.History {
			fields = append(fields, "input[].internal_chat_message_metadata_passthrough.turn_id")
		}
		diagnostic.Changes = append(diagnostic.Changes, turnMapping.identityChange(original, outbound, fields...))
		plan.claims = append(plan.claims, database.CodexIdentityAliasClaim{
			AliasKey:  codexIdentityDigest("codex-account-alias-v1", outbound),
			SourceKey: codexIdentityDigest("codex-account-turn-source-v1", mapping.owner, mapping.account, turnMapping.epoch, original),
		})
		plan.references[referenceKey] = turnEpoch
		if !bound {
			plan.epochs[identityKey] = turnEpoch
		}
	}
	return plan, nil
}

func (mapping *codexAccountIdentity) rewriteTurnValue(original string) string {
	if alias := mapping.turnAliases[strings.TrimSpace(original)]; alias != "" {
		return alias
	}
	if parsed, err := uuid.Parse(strings.TrimSpace(original)); err == nil {
		if alias := mapping.turnAliases[parsed.String()]; alias != "" {
			return alias
		}
	}
	return original
}
