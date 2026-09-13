package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestNewID_Unique(t *testing.T) {
	a := NewID()
	b := NewID()
	if a == b {
		t.Fatalf("NewID returned the same value twice: %s", a)
	}
	if len(a) != 32 {
		t.Fatalf("NewID length = %d, want 32", len(a))
	}
}

func TestEncodeDecodeRoundTrip(t *testing.T) {
	cmd := Command{ID: NewID(), Action: "status"}
	env := cmd.ToEnvelope()
	raw, err := Encode(env)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	got, err := DecodeEnvelope(raw)
	if err != nil {
		t.Fatalf("DecodeEnvelope: %v", err)
	}
	if got.Type != TypeCommand || got.ID != cmd.ID || got.Action != cmd.Action {
		t.Fatalf("roundtrip mismatch: %+v", got)
	}
	got2, err := CommandFromEnvelope(got)
	if err != nil {
		t.Fatalf("CommandFromEnvelope: %v", err)
	}
	if got2.ID != cmd.ID {
		t.Fatalf("id lost in roundtrip")
	}
}

func TestResultEnvelopesSuccess(t *testing.T) {
	yes := true
	env := &Envelope{Type: TypeResult, ID: "abc", Success: &yes, Error: "boom"}
	r, err := ResultFromEnvelope(env)
	if err != nil {
		t.Fatalf("ResultFromEnvelope: %v", err)
	}
	if !r.Success || r.Error != "boom" {
		t.Fatalf("unexpected result: %+v", r)
	}
}

func TestResultEnvelopesFailure(t *testing.T) {
	no := false
	env := &Envelope{Type: TypeResult, ID: "abc", Success: &no}
	r, err := ResultFromEnvelope(env)
	if err != nil {
		t.Fatalf("ResultFromEnvelope: %v", err)
	}
	if r.Success {
		t.Fatalf("expected success=false")
	}
}

func TestDecodeEnvelope_RejectsEmptyType(t *testing.T) {
	raw := []byte(`{"id":"x"}`)
	if _, err := DecodeEnvelope(raw); err == nil {
		t.Fatal("expected error for missing type")
	}
}

func TestDecodeEnvelope_RejectsGarbage(t *testing.T) {
	if _, err := DecodeEnvelope([]byte("not-json")); err == nil {
		t.Fatal("expected error for non-json")
	}
}

func TestHelloRoundTrip(t *testing.T) {
	h := Hello{ClientID: "client-1", Token: "tok", Version: "0.1.0", Hostname: "h"}
	raw, err := Encode(h.ToEnvelope())
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	env, err := DecodeEnvelope(raw)
	if err != nil {
		t.Fatalf("DecodeEnvelope: %v", err)
	}
	h2, err := HelloFromEnvelope(env)
	if err != nil {
		t.Fatalf("HelloFromEnvelope: %v", err)
	}
	if h2.ClientID != h.ClientID || h2.Token != h.Token || h2.Hostname != h.Hostname {
		t.Fatalf("hello roundtrip lost fields: %+v", h2)
	}
}

func TestCommandRejectsMissingFields(t *testing.T) {
	cases := []*Envelope{
		{Type: TypeCommand, Action: "status"},
		{Type: TypeCommand, ID: "x"},
	}
	for i, env := range cases {
		if _, err := CommandFromEnvelope(env); err == nil {
			t.Fatalf("case %d: expected error", i)
		}
	}
}

func TestResultWithDataPreservesJSON(t *testing.T) {
	data, _ := json.Marshal(map[string]string{"stdout": "hi"})
	r := Result{ID: "x", Success: true, Data: data}
	raw, err := Encode(r.ToEnvelope())
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if !strings.Contains(string(raw), `"stdout":"hi"`) {
		t.Fatalf("data not embedded: %s", raw)
	}
}

func TestToolListRoundTrip(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{}}`)
	tl := ToolList{Tools: []ToolMeta{
		{Name: "status", Description: "Get status", InputSchema: schema},
		{Name: "exec", Description: "Run cmd", InputSchema: schema},
	}}
	raw, err := Encode(tl.ToEnvelope())
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	env, err := DecodeEnvelope(raw)
	if err != nil {
		t.Fatalf("DecodeEnvelope: %v", err)
	}
	if env.Type != TypeToolList {
		t.Fatalf("type = %s, want toollist", env.Type)
	}
	got, err := ToolListFromEnvelope(env)
	if err != nil {
		t.Fatalf("ToolListFromEnvelope: %v", err)
	}
	if len(got.Tools) != 2 {
		t.Fatalf("tools = %d, want 2", len(got.Tools))
	}
	if got.Tools[0].Name != "status" {
		t.Fatalf("first tool = %s", got.Tools[0].Name)
	}
	if !strings.Contains(string(raw), `"inputSchema"`) {
		t.Fatalf("schema not in wire payload: %s", raw)
	}
}

func TestToolListFromEnvelope_RejectsWrongType(t *testing.T) {
	env := &Envelope{Type: TypeCommand}
	if _, err := ToolListFromEnvelope(env); err == nil {
		t.Fatal("expected error for wrong envelope type")
	}
}
