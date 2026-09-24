package proxy

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/tidwall/gjson"
)

// Only protocol shape and identifier hashes are captured. Never collect tool
// descriptions, schemas, arguments, outputs or conversation text here.
type toolProtocolItem struct {
	Path            string `json:"path"`
	Kind            string `json:"kind"`
	NameState       string `json:"name_state"`
	NameHash        string `json:"name_hash,omitempty"`
	NestedNameState string `json:"nested_name_state,omitempty"`
	NestedNameHash  string `json:"nested_name_hash,omitempty"`
	NamespaceState  string `json:"namespace_state"`
	NamespaceHash   string `json:"namespace_hash,omitempty"`
	Issue           string `json:"issue,omitempty"`
}

type toolProtocolDiagnostic struct {
	Declarations       int                `json:"declarations"`
	Calls              int                `json:"calls"`
	MissingNames       int                `json:"missing_names"`
	MissingNamespaces  int                `json:"missing_namespaces"`
	DeclarationSamples []toolProtocolItem `json:"declaration_samples,omitempty"`
	CallSamples        []toolProtocolItem `json:"call_samples,omitempty"`
	Issues             []toolProtocolItem `json:"issues,omitempty"`
	OmittedIssues      int                `json:"omitted_issues,omitempty"`
	ScanTruncated      bool               `json:"scan_truncated,omitempty"`
}

func toolIdentifierState(value gjson.Result) (string, string) {
	if !value.Exists() {
		return "absent", ""
	}
	if value.Type != gjson.String {
		return "invalid_type", ""
	}
	if strings.TrimSpace(value.String()) == "" {
		return "empty", ""
	}
	digest := sha256.Sum256([]byte(value.String()))
	return "present", hex.EncodeToString(digest[:12])
}

func toolProtocolShape(item gjson.Result, path, kind string) toolProtocolItem {
	shape := toolProtocolItem{Path: path, Kind: kind}
	shape.NameState, shape.NameHash = toolIdentifierState(item.Get("name"))
	shape.NamespaceState, shape.NamespaceHash = toolIdentifierState(item.Get("namespace"))
	for _, field := range []string{"function", "custom"} {
		if item.Get(field).IsObject() {
			shape.NestedNameState, shape.NestedNameHash = toolIdentifierState(item.Get(field + ".name"))
			break
		}
	}
	return shape
}

func (d *toolProtocolDiagnostic) addIssue(item toolProtocolItem) {
	if len(d.Issues) < 8 {
		d.Issues = append(d.Issues, item)
	} else {
		d.OmittedIssues++
	}
}

func diagnoseToolProtocol(body []byte) *toolProtocolDiagnostic {
	root := gjson.ParseBytes(body)
	d := &toolProtocolDiagnostic{}
	// Match by hash to avoid retaining identifiers in the diagnostic. A default
	// declaration makes an unqualified call valid even if a namespaced twin exists.
	defaultNames, namespacedNames := map[string]bool{}, map[string]bool{}
	visited := 0
	var declarations func(gjson.Result, string, bool, int)
	declarations = func(tools gjson.Result, path string, namespaced bool, depth int) {
		if depth > 8 {
			d.ScanTruncated = true
			return
		}
		tools.ForEach(func(index, tool gjson.Result) bool {
			visited++
			if visited > 4096 {
				d.ScanTruncated = true
				return false
			}
			kind := tool.Get("type").String()
			if kind == "" && (tool.Get("function").IsObject() || tool.Get("name").Exists()) {
				kind = "function"
			}
			if kind != "function" && kind != "custom" && kind != "namespace" {
				return true
			}
			entry := toolProtocolShape(tool, fmt.Sprintf("%s[%d]", path, index.Int()), kind)
			d.Declarations++
			if len(d.DeclarationSamples) < 8 {
				d.DeclarationSamples = append(d.DeclarationSamples, entry)
			}
			nameHash := entry.NameHash
			if nameHash == "" {
				nameHash = entry.NestedNameHash
			}
			if nameHash == "" {
				d.MissingNames++
				entry.Issue = "missing_name"
				d.addIssue(entry)
			}
			if kind == "namespace" {
				declarations(tool.Get("tools"), entry.Path+".tools", true, depth+1)
			} else if nameHash != "" {
				if namespaced || entry.NamespaceState == "present" {
					namespacedNames[nameHash] = true
				} else {
					defaultNames[nameHash] = true
				}
			}
			return true
		})
	}
	declarations(root.Get("tools"), "tools", false, 0)
	root.Get("input").ForEach(func(index, item gjson.Result) bool {
		if kind := item.Get("type").String(); kind == "additional_tools" || kind == "tool_search_output" {
			declarations(item.Get("tools"), fmt.Sprintf("input[%d].tools", index.Int()), false, 0)
		}
		return visited <= 4096
	})
	call := func(item gjson.Result, path, kind string) {
		visited++
		if visited > 4096 {
			d.ScanTruncated = true
			return
		}
		d.Calls++
		entry := toolProtocolShape(item, path, kind)
		if len(d.CallSamples) < 8 {
			d.CallSamples = append(d.CallSamples, entry)
		}
		nameHash := entry.NameHash
		if nameHash == "" {
			nameHash = entry.NestedNameHash
		}
		if nameHash == "" {
			d.MissingNames++
			entry.Issue = "missing_name"
			d.addIssue(entry)
		} else if entry.NamespaceState != "present" && namespacedNames[nameHash] && !defaultNames[nameHash] {
			d.MissingNamespaces++
			entry.Issue = "missing_namespace"
			d.addIssue(entry)
		}
	}
	root.Get("input").ForEach(func(index, item gjson.Result) bool {
		if kind := item.Get("type").String(); kind == "function_call" || kind == "custom_tool_call" {
			call(item, fmt.Sprintf("input[%d]", index.Int()), kind)
		}
		return visited <= 4096
	})
	root.Get("messages").ForEach(func(index, message gjson.Result) bool {
		message.Get("tool_calls").ForEach(func(callIndex, item gjson.Result) bool {
			kind := "function_call"
			if item.Get("type").String() == "custom" {
				kind = "custom_tool_call"
			}
			call(item, fmt.Sprintf("messages[%d].tool_calls[%d]", index.Int(), callIndex.Int()), kind)
			return visited <= 4096
		})
		return visited <= 4096
	})
	if d.Declarations == 0 && d.Calls == 0 && !d.ScanTruncated {
		return nil
	}
	return d
}
