package actions

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"tomcat-ops-helper/internal/config"
	"tomcat-ops-helper/internal/dbop"
)

type formTestOp struct {
	dbop.Op
	data []byte
}

func (o formTestOp) ReadFormEntity(ctx context.Context, id string) ([]byte, error) {
	return o.data, ctx.Err()
}
func (o formTestOp) ReadOnly() bool { return true }

func TestFormDispatch(t *testing.T) {
	r := NewRegistry(&config.HelperConfig{}, nil)
	has := func() bool {
		for _, d := range r.Tools() {
			if d.Name == "form_content" {
				if !json.Valid(d.InputSchema) {
					t.Fatal("invalid schema")
				}
				return true
			}
		}
		return false
	}
	if has() {
		t.Fatal("advertised without ERP")
	}
	if _, err := r.Dispatch(context.Background(), "form_content", json.RawMessage(`{"form_id":"619fhq907y4pqbl1"}`)); err == nil {
		t.Fatal("missing ERP accepted")
	}
	var b bytes.Buffer
	w := zip.NewWriter(&b)
	f, _ := w.Create("content/main.js")
	f.Write([]byte("你好abc"))
	w.Close()
	r.WithDB("erp_db_operator", formTestOp{data: append([]byte("MJU"), b.Bytes()...)})
	if !has() {
		t.Fatal("missing metadata")
	}
	raw, err := r.Dispatch(context.Background(), "form_content", json.RawMessage(`{"form_id":"619fhq907y4pqbl1","path":"content/main.js","offset":1,"max_chars":2}`))
	if err != nil {
		t.Fatal(err)
	}
	var out dbop.FormContent
	json.Unmarshal(raw, &out)
	if out.Text != "好a" || out.NextOffset == nil || *out.NextOffset != 3 {
		t.Fatalf("bad page: %s", raw)
	}
	// VRS uses the same read-only ERP connector and Unicode pagination.
	r.WithDB("erp_db_operator", formTestOp{data: append([]byte("VRS"), b.Bytes()...)})
	for _, tool := range r.Tools() {
		if tool.Name == "form_content" && !strings.Contains(tool.Description, "VRS") {
			t.Fatal("metadata does not advertise VRS")
		}
	}
	raw, err = r.Dispatch(context.Background(), "form_content", json.RawMessage(`{"form_id":"3dp3aozb3u6rko95","path":"content/main.js","offset":1,"max_chars":2}`))
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if out.Prefix != "VRS" || out.Text != "好a" || out.NextOffset == nil || *out.NextOffset != 3 {
		t.Fatalf("bad VRS page: %s", raw)
	}
	for _, args := range []string{`{"form_id":"bad"}`, `{"form_id":"619fhq907y4pqbl1","offset":0.5}`, `{"form_id":"619fhq907y4pqbl1","max_chars":0}`, `{"form_id":"619fhq907y4pqbl1","encoding":"bad"}`} {
		if _, err := r.Dispatch(context.Background(), "form_content", json.RawMessage(args)); err == nil {
			t.Fatalf("accepted %s", args)
		}
	}
}
