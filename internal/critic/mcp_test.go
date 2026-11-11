package critic

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestMCPListsToolsAndFailsSoftWithoutAnEstate(t *testing.T) {
	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"list_judgements","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"nope"}`,
	}, "\n") + "\n"
	var out bytes.Buffer
	if err := ServeMCP(t.TempDir(), strings.NewReader(in), &out); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 4 {
		t.Fatalf("a response per request, none for the notification: %d\n%s", len(lines), out.String())
	}
	var tools struct {
		Result struct{ Tools []mcpTool } `json:"result"`
	}
	json.Unmarshal([]byte(lines[1]), &tools)
	if len(tools.Result.Tools) != len(mcpTools) {
		t.Fatalf("tools/list: %s", lines[1])
	}
	if !strings.Contains(lines[2], `"isError":true`) || !strings.Contains(lines[2], "no running estate") {
		t.Fatalf("a tool error is a result with isError: %s", lines[2])
	}
	if !strings.Contains(lines[3], `"error"`) {
		t.Fatalf("an unknown method is a JSON-RPC error: %s", lines[3])
	}
}
