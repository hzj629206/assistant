package agent

import (
	"context"
	"encoding/json"
	"log"
	"strings"

	apprpc "github.com/pmenglund/codex-sdk-go/rpc"
)

// appServerApprovalTransport is a temporary workaround for codex-sdk-go v0.167.2.
// Codex CLI v0.171.0 emits execpolicy_amendment and network_policy_amendment inside
// availableDecisions, but this SDK expects execpolicyAmendment and
// networkPolicyAmendment. Without normalization, valid command approval requests
// fail with JSON-RPC -32602 before reaching the approval callbacks.
//
// Remove this wrapper, its normalization helpers and helper-only tests, and the
// wrapping call in newAppServerRPCClient once an upgraded SDK passes
// TestAppServerRPCDispatchesApprovalRequests with the raw transport and original
// snake_case fixtures. Retain those RPC regression tests and the approval-policy
// callbacks, which are independent of this compatibility workaround.
type appServerApprovalTransport struct {
	apprpc.Transport
}

func (t appServerApprovalTransport) ReadLine() (string, error) {
	line, err := t.Transport.ReadLine()
	if err != nil {
		return line, err
	}
	return normalizeAppServerCommandApproval(line), nil
}

func (t appServerApprovalTransport) WriteLineContext(ctx context.Context, line string) error {
	if transport, ok := t.Transport.(apprpc.ContextTransport); ok {
		return transport.WriteLineContext(ctx, line)
	}
	return t.WriteLine(line)
}

func normalizeAppServerCommandApproval(line string) string {
	if !strings.Contains(line, "item/commandExecution/requestApproval") || !json.Valid([]byte(line)) {
		return line
	}
	decoder := json.NewDecoder(strings.NewReader(line))
	decoder.UseNumber()
	var message map[string]any
	if err := decoder.Decode(&message); err != nil || message["method"] != "item/commandExecution/requestApproval" {
		return line
	}
	// log.Printf("app-server runner raw command approval request: request=%s", line)
	params, _ := message["params"].(map[string]any)
	decisions, _ := params["availableDecisions"].([]any)
	changed := false
	for _, value := range decisions {
		decision, _ := value.(map[string]any)
		execPolicy, _ := decision["acceptWithExecpolicyAmendment"].(map[string]any)
		if normalizeAppServerDecisionField(execPolicy, "execpolicy_amendment", "execpolicyAmendment") {
			changed = true
		}
		networkPolicy, _ := decision["applyNetworkPolicyAmendment"].(map[string]any)
		if normalizeAppServerDecisionField(networkPolicy, "network_policy_amendment", "networkPolicyAmendment") {
			changed = true
		}
	}
	if !changed {
		return line
	}
	payload, err := json.Marshal(message)
	if err != nil {
		return line
	}
	log.Printf("app-server runner normalized structured command approval decision fields for SDK compatibility")
	return string(payload)
}

func normalizeAppServerDecisionField(fields map[string]any, wireName, sdkName string) bool {
	value, ok := fields[wireName]
	if !ok {
		return false
	}
	if _, exists := fields[sdkName]; exists {
		return false
	}
	fields[sdkName] = value
	return true
}
