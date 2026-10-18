package dbop

import (
	"archive/zip"
	"bytes"
	"context"
	"golang.org/x/text/encoding/simplifiedchinese"
	"strings"
	"testing"
)

func formArchive(t *testing.T, prefix string, content []byte) []byte {
	t.Helper()
	var b bytes.Buffer
	w := zip.NewWriter(&b)
	f, e := w.Create("content/main.js")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = f.Write(content); e != nil {
		t.Fatal(e)
	}
	if e = w.Close(); e != nil {
		t.Fatal(e)
	}
	return append([]byte(prefix), b.Bytes()...)
}
func TestFormArchive(t *testing.T) {
	for _, prefix := range []string{"", "MJU", "VRS", "JCU", "FHA", "VHS", "XYZ"} {
		data := formArchive(t, prefix, []byte("你好abc"))
		result, err := InspectForm(context.Background(), data, "", "auto", 0, 2)
		if err != nil || len(result.Files) != 1 || result.Text != "" || result.Prefix != prefix {
			t.Fatalf("list: %+v %v", result, err)
		}
		result, err = InspectForm(context.Background(), data, "content/main.js", "auto", 1, 2)
		if err != nil || result.Text != "好a" || !result.Truncated || result.NextOffset == nil || *result.NextOffset != 3 {
			t.Fatalf("page: %+v %v", result, err)
		}
		result, err = InspectForm(context.Background(), data, "content/main.js", "auto", 0, 100)
		if err != nil || result.Text != "你好abc" || result.Truncated || result.TotalChars != 5 {
			t.Fatalf("full text: %+v %v", result, err)
		}
	}
}
func TestFormGB18030(t *testing.T) {
	raw, err := simplifiedchinese.GB18030.NewEncoder().Bytes([]byte("库存查询"))
	if err != nil {
		t.Fatal(err)
	}
	result, err := InspectForm(context.Background(), formArchive(t, "MJU", raw), "content/main.js", "auto", 0, 100)
	if err != nil || result.Text != "库存查询" || result.Encoding != "gb18030" {
		t.Fatalf("decode: %+v %v", result, err)
	}
}
func TestFormRejectsInvalidInput(t *testing.T) {
	data := formArchive(t, "MJU", []byte("abc"))
	for _, tc := range []struct {
		data           []byte
		path, encoding string
		offset, limit  int
	}{
		{[]byte("bad"), "", "auto", 0, 1},
		{[]byte("VR"), "", "auto", 0, 1},
		{[]byte("VRS"), "", "auto", 0, 1},
		{[]byte("VRSnot-zip"), "", "auto", 0, 1},
		{formArchive(t, "AB", []byte("abc")), "", "auto", 0, 1},
		{formArchive(t, "ABCD", []byte("abc")), "", "auto", 0, 1},
		{formArchive(t, "aBC", []byte("abc")), "", "auto", 0, 1},
		{formArchive(t, "A1Z", []byte("abc")), "", "auto", 0, 1},
		{formArchive(t, "A!Z", []byte("abc")), "", "auto", 0, 1},
		{formArchive(t, "中", []byte("abc")), "", "auto", 0, 1},
		{[]byte("XYZPK\x03\x04broken"), "", "auto", 0, 1},
		{[]byte("PK\x05\x06"), "", "auto", 0, 1},
		{data, "missing", "auto", 0, 1},
		{data, "content/main.js", "auto", 4, 1},
		{data, "", "auto", -1, 1},
		{data, "", "auto", 0, 64001},
		{data, "", "bad", 0, 1},
		{formArchive(t, "", []byte{0, 1}), "content/main.js", "auto", 0, 1},
	} {
		if _, err := InspectForm(context.Background(), tc.data, tc.path, tc.encoding, tc.offset, tc.limit); err == nil {
			t.Fatalf("expected rejection: %+v", tc)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := InspectForm(ctx, data, "", "auto", 0, 1); err == nil {
		t.Fatal("ignored cancellation")
	}
}
func TestFormExpandedLimit(t *testing.T) {
	data := formArchive(t, "MJU", []byte(strings.Repeat("x", maxFormExpanded+1)))
	if _, err := InspectForm(context.Background(), data, "", "auto", 0, 1); err == nil {
		t.Fatal("accepted oversized expansion")
	}
}
func TestReadFormRejectsUnsupportedConnection(t *testing.T) {
	for _, tc := range []struct{ driver, id string }{{"mysql", "619fhq907y4pqbl1"}, {"oracle", "bad"}} {
		o := &SQLOp{driver: tc.driver}
		if _, err := o.ReadFormEntity(context.Background(), tc.id); err == nil {
			t.Fatal("expected rejection")
		}
	}
}

func TestFormCRCAndDuplicates(t *testing.T) {
	var b bytes.Buffer
	w := zip.NewWriter(&b)
	f, _ := w.CreateHeader(&zip.FileHeader{Name: "a", Method: zip.Store})
	f.Write([]byte("unique-payload"))
	w.Close()
	data := b.Bytes()
	i := bytes.Index(data, []byte("unique-payload"))
	data[i] ^= 1
	for _, prefix := range []string{"", "MJU", "VRS", "JCU", "FHA", "VHS", "XYZ"} {
		payload := append([]byte(prefix), data...)
		if _, e := InspectForm(context.Background(), payload, "", "auto", 0, 10); e != nil {
			t.Fatalf("directory listing should not read member CRC: %s: %v", prefix, e)
		}
		if _, e := InspectForm(context.Background(), payload, "a", "auto", 0, 10); e == nil {
			t.Fatalf("ignored CRC: %s", prefix)
		}
	}
	b.Reset()
	w = zip.NewWriter(&b)
	w.Create("a")
	w.Create("a")
	w.Close()
	if _, e := InspectForm(context.Background(), b.Bytes(), "", "auto", 0, 10); e == nil {
		t.Fatal("ignored duplicate")
	}
}

func TestFormEmptyZIPAndDirectories(t *testing.T) {
	for _, directory := range []bool{false, true} {
		var b bytes.Buffer
		w := zip.NewWriter(&b)
		if directory {
			if _, err := w.Create("content/"); err != nil {
				t.Fatal(err)
			}
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		for _, prefix := range []string{"", "MJU", "VRS", "JCU", "FHA", "VHS", "XYZ"} {
			result, err := InspectForm(context.Background(), append([]byte(prefix), b.Bytes()...), "", "auto", 0, 10)
			if err != nil || result.Prefix != prefix {
				t.Fatalf("prefix %q directory %v: %+v %v", prefix, directory, result, err)
			}
			if directory && (len(result.Files) != 1 || !result.Files[0].Directory) {
				t.Fatalf("bad directory listing: %+v", result)
			}
		}
	}
}
