package agent

import (
	"encoding/json"
	"testing"

	"github.com/pmenglund/codex-sdk-go/protocol"
)

func TestNormalizeAppServerCommandApprovalPreservesPayload(t *testing.T) {
	t.Parallel()

	line := `{"id":9007199254740993,"method":"item/commandExecution/requestApproval","params":{
		"threadId":"thread-1","turnId":"turn-1","itemId":"item-1",
		"command":"touch /opt/output/result.txt",
		"additionalPermissions":{"fileSystem":{"write":["/opt/output"]}},
		"availableDecisions":["decline",{
			"acceptWithExecpolicyAmendment":{"execpolicy_amendment":["touch"]}
		}]
	}}`
	var request struct {
		ID     json.RawMessage                                `json:"id"`
		Params protocol.CommandExecutionRequestApprovalParams `json:"params"`
	}
	if err := json.Unmarshal([]byte(normalizeAppServerCommandApproval(line)), &request); err != nil {
		t.Fatalf("decode normalized approval with SDK: %v", err)
	}
	if string(request.ID) != "9007199254740993" {
		t.Fatalf("request ID changed: %s", request.ID)
	}
	if request.Params.Command == nil || *request.Params.Command != "touch /opt/output/result.txt" {
		t.Fatalf("command changed: %#v", request.Params.Command)
	}
	if !appServerRequestsAdditionalWrite(request.Params.AdditionalPermissions) {
		t.Fatal("write permission metadata was lost")
	}
	if len(request.Params.AvailableDecisions) != 2 ||
		request.Params.AvailableDecisions[1].Kind() != protocol.CommandExecutionApprovalDecisionKindAcceptWithExecpolicyAmendment {
		t.Fatalf("approval choices changed: %#v", request.Params.AvailableDecisions)
	}
}

func TestNormalizeAppServerCommandApprovalLeavesOtherMessagesUnchanged(t *testing.T) {
	t.Parallel()

	for _, line := range []string{
		`{"id":1,"method":"item/tool/call","params":{"availableDecisions":[{"acceptWithExecpolicyAmendment":{"execpolicy_amendment":["touch"]}}]}}`,
		`{"id":1,"method":"item/commandExecution/requestApproval","params":{"availableDecisions":["accept","decline"]}}`,
		`{"id":1,"method":"item/commandExecution/requestApproval","params":{"availableDecisions":[{"acceptWithExecpolicyAmendment":{"execpolicyAmendment":["touch"]}}]}}`,
		`{"id":1,"method":"item/commandExecution/requestApproval","params":{"availableDecisions":[{"futureDecision":{"execpolicy_amendment":["touch"]}}]}}`,
		`{"id":1,"method":"item/commandExecution/requestApproval","params":{}} trailing garbage`,
	} {
		if got := normalizeAppServerCommandApproval(line); got != line {
			t.Errorf("unexpected normalization: got %s, want %s", got, line)
		}
	}
}
