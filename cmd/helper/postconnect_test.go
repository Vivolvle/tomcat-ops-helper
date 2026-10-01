package main

import (
	"encoding/json"
	"testing"

	"tomcat-ops-helper/internal/dbop"
	"tomcat-ops-helper/internal/protocol"
)

// fakeConn captures what PostConnect writes, standing in for the live
// websocket.
type fakeConn struct {
	written []any
	err     error
}

func (f *fakeConn) WriteJSON(v any) error {
	f.written = append(f.written, v)
	return f.err
}

// TestPostConnectToolList_IsValidJSON (todo E2): the 2026-09-14 field bug
// (E1) was an unescaped quote in a tool description that only exploded
// when the server decoded the ToolList frame PostConnect sends — after
// the helper was already deployed. Static schema tests (in actions)
// didn't cover the wire path. This test runs the real postConnectToolList
// closure and proves the envelope it writes is decodable and every
// InputSchema is a valid JSON object — including dbop tools, which only
// appear after WithDB.
func TestPostConnectToolList_IsValidJSON(t *testing.T) {
	f := newFixture(t)
	f.registry.WithDB("esb_db_operator", &dbop.FakeOp{})

	conn := &fakeConn{}
	if err := postConnectToolList(f.registry)(conn); err != nil {
		t.Fatalf("postConnect: %v", err)
	}
	if len(conn.written) != 1 {
		t.Fatalf("expected exactly one envelope, got %d", len(conn.written))
	}

	env, ok := conn.written[0].(*protocol.Envelope)
	if !ok {
		t.Fatalf("written value is %T, want *protocol.Envelope", conn.written[0])
	}
	if env.Type != protocol.TypeToolList {
		t.Fatalf("envelope type = %q, want %q", env.Type, protocol.TypeToolList)
	}

	tl, err := protocol.ToolListFromEnvelope(env)
	if err != nil {
		t.Fatalf("ToolListFromEnvelope (this is the frame the server decodes): %v", err)
	}
	if len(tl.Tools) == 0 {
		t.Fatal("tool list is empty")
	}

	sawDBTool := false
	for _, m := range tl.Tools {
		var schema map[string]any
		if err := json.Unmarshal(m.InputSchema, &schema); err != nil {
			t.Fatalf("tool %q inputSchema is not valid JSON: %v\nschema: %s", m.Name, err, m.InputSchema)
		}
		if m.Name == "esb_db_operator" {
			sawDBTool = true
		}
	}
	if !sawDBTool {
		t.Fatal("esb_db_operator missing from pushed ToolList; dbop schemas must be covered too")
	}
}

// TestToolMetas_MatchesTools sanity-checks that both ToolList paths
// (PostConnect and reload push) advertise the same set.
func TestToolMetas_MatchesTools(t *testing.T) {
	f := newFixture(t)
	f.registry.WithDB("erp_db_operator", &dbop.FakeOp{})

	tools := f.registry.Tools()
	meta := toolMetas(f.registry)
	if len(tools) != len(meta) {
		t.Fatalf("toolMetas len %d != Tools len %d", len(meta), len(tools))
	}
	for i := range tools {
		if tools[i].Name != meta[i].Name {
			t.Fatalf("position %d: %q vs %q — tool lists diverged", i, tools[i].Name, meta[i].Name)
		}
	}
}
