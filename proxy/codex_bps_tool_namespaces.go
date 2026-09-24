package proxy

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type bpsNamespaceRepairDiagnostic struct {
	ScanTruncated bool               `json:"scan_truncated,omitempty"`
	Repaired      int                `json:"repaired"`
	Unresolved    int                `json:"unresolved"`
	Samples       []toolProtocolItem `json:"samples,omitempty"`
}

// Some clients round-trip calls without namespace. Restore it only when every
// declaration for that name and call kind agrees on one explicit namespace.
// Never rewrite arguments, results, qualified calls, or ambiguous/default tools.
func repairBPSToolNamespaces(body []byte) ([]byte, *bpsNamespaceRepairDiagnostic, error) {
	type binding struct {
		namespace string
		ambiguous bool
	}
	bindings := map[string]binding{}
	visited, truncated := 0, false
	var collect func(gjson.Result, string, int)
	collect = func(tools gjson.Result, namespace string, depth int) {
		tools.ForEach(func(_, tool gjson.Result) bool {
			visited++
			if visited > 4096 {
				truncated = true
				return false
			}
			kind, name := tool.Get("type").String(), tool.Get("name").String()
			if kind == "namespace" {
				if depth < 8 {
					collect(tool.Get("tools"), name, depth+1)
				} else {
					truncated = true
				}
				return true
			}
			if (kind != "function" && kind != "custom") || strings.TrimSpace(name) == "" {
				return true
			}
			ns := namespace
			if explicit := tool.Get("namespace"); explicit.Type == gjson.String {
				ns = explicit.String()
			}
			key := kind + ":" + name
			candidate := binding{namespace: ns, ambiguous: depth > 1 || strings.TrimSpace(ns) == ""}
			if previous, ok := bindings[key]; ok {
				candidate.ambiguous = candidate.ambiguous || previous.ambiguous || previous.namespace != ns
			}
			bindings[key] = candidate
			return true
		})
	}
	root := gjson.ParseBytes(body)
	collect(root.Get("tools"), "", 0)
	items := root.Get("input").Array()
	for _, item := range items {
		if kind := item.Get("type").String(); kind == "additional_tools" || kind == "tool_search_output" {
			collect(item.Get("tools"), "", 0)
		}
	}
	d := &bpsNamespaceRepairDiagnostic{}
	if truncated {
		d.ScanTruncated = true
		return body, d, nil
	}
	out := make([]json.RawMessage, len(items))
	for i, item := range items {
		out[i] = json.RawMessage(item.Raw)
		kind := item.Get("type").String()
		if kind != "function_call" && kind != "custom_tool_call" || item.Get("namespace").Exists() {
			continue
		}
		declarationKind := "function"
		if kind == "custom_tool_call" {
			declarationKind = "custom"
		}
		candidate, ok := bindings[declarationKind+":"+item.Get("name").String()]
		if !ok || candidate.ambiguous {
			d.Unresolved++
			continue
		}
		updated, err := sjson.SetBytes(out[i], "namespace", candidate.namespace)
		if err != nil {
			return nil, nil, err
		}
		out[i] = updated
		d.Repaired++
		if len(d.Samples) < 8 {
			entry := toolProtocolShape(gjson.ParseBytes(updated), fmt.Sprintf("input[%d]", i), kind)
			entry.Issue = "namespace_restored_from_unique_declaration"
			d.Samples = append(d.Samples, entry)
		}
	}
	if d.Repaired == 0 {
		return body, d, nil
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		return nil, nil, err
	}
	updated, err := sjson.SetRawBytes(body, "input", encoded)
	return updated, d, err
}
