package actions

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// uploadOnce drives one chunk through the action. Returns the decoded
// Result data. Caller decides whether the chunk is final.
func uploadOnce(t *testing.T, reg *Registry, id, path string, offset int64, chunk []byte, done bool, sha string, overwrite bool) (UploadProgressData, UploadResultData) {
	t.Helper()
	b64 := base64.StdEncoding.EncodeToString(chunk)
	params := mustMarshal(map[string]any{
		"path":             path,
		"upload_id":        id,
		"chunk":            b64,
		"chunk_offset":     offset,
		"chunk_done":       done,
		"expected_sha256":  sha,
		"overwrite":        overwrite,
	})
	raw, err := reg.uploadFile(params)
	if err != nil {
		t.Fatalf("uploadFile (offset=%d done=%v): %v", offset, done, err)
	}
	var prog UploadProgressData
	var fin UploadResultData
	if done {
		_ = json.Unmarshal(raw, &fin)
	} else {
		_ = json.Unmarshal(raw, &prog)
	}
	return prog, fin
}

func TestUploadFile_SingleChunk_Success(t *testing.T) {
	cfg := mustCfg(t)
	reg := NewRegistry(cfg, mustExec(t))
	dst := filepath.Join(cfg.LogDir, "small.bin")
	body := []byte("hello world")
	sum := sha256Hex(body)

	_, fin := uploadOnce(t, reg, "u-1", dst, 0, body, true, sum, false)
	if fin.Bytes != int64(len(body)) {
		t.Fatalf("bytes = %d, want %d", fin.Bytes, len(body))
	}
	if fin.SHA256 != sum {
		t.Fatalf("sha = %s, want %s", fin.SHA256, sum)
	}
	if !fin.Verified {
		t.Fatalf("verified should be true when expected matches")
	}
	if fin.Overwritten {
		t.Fatalf("overwritten should be false on first write")
	}
	onDisk, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("readback: %v", err)
	}
	if !bytes.Equal(onDisk, body) {
		t.Fatalf("readback mismatch: %q vs %q", onDisk, body)
	}
	if _, err := os.Stat(dst + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf(".tmp leftover: %v", err)
	}
}

func TestUploadFile_MultiChunk_AssemblesAndHashes(t *testing.T) {
	cfg := mustCfg(t)
	reg := NewRegistry(cfg, mustExec(t))
	dst := filepath.Join(cfg.LogDir, "big.bin")

	// Three 100-byte chunks → 300 bytes total.
	body := bytes.Repeat([]byte("ABCD"), 75) // 300 bytes
	sum := sha256Hex(body)
	parts := [][]byte{body[0:100], body[100:200], body[200:300]}

	const id = "u-multi"
	uploadOnce(t, reg, id, dst, 0, parts[0], false, "", false)
	prog, _ := uploadOnce(t, reg, id, dst, 100, parts[1], false, "", false)
	if prog.BytesWritten != 200 {
		t.Fatalf("progress.bytes_written = %d, want 200", prog.BytesWritten)
	}
	_, fin := uploadOnce(t, reg, id, dst, 200, parts[2], true, sum, false)
	if fin.Bytes != 300 {
		t.Fatalf("final bytes = %d, want 300", fin.Bytes)
	}
	if fin.SHA256 != sum {
		t.Fatalf("final sha = %s, want %s", fin.SHA256, sum)
	}
	onDisk, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("readback: %v", err)
	}
	if !bytes.Equal(onDisk, body) {
		t.Fatalf("readback mismatch")
	}
}

func TestUploadFile_OffsetMismatch_Aborts(t *testing.T) {
	cfg := mustCfg(t)
	reg := NewRegistry(cfg, mustExec(t))
	dst := filepath.Join(cfg.LogDir, "out.bin")

	uploadOnce(t, reg, "u-mm", dst, 0, []byte("ABC"), false, "", false)
	// Skip chunk #2, send offset 200 instead.
	_, err := reg.uploadFile(mustMarshal(map[string]any{
		"path":         dst,
		"upload_id":    "u-mm",
		"chunk":        base64.StdEncoding.EncodeToString([]byte("XYZ")),
		"chunk_offset": int64(200),
		"chunk_done":   true,
	}))
	if err == nil {
		t.Fatal("expected error on offset mismatch")
	}
	if !strings.Contains(err.Error(), "aborted") {
		t.Fatalf("error = %v, want 'aborted' substring", err)
	}
	// .tmp should be gone and no final file.
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Fatalf("dst should not exist: %v", err)
	}
	if _, err := os.Stat(dst + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf(".tmp should be removed: %v", err)
	}

	// After abort, a fresh upload_id should succeed at offset 0.
	body := []byte("fresh start")
	_, fin := uploadOnce(t, reg, "u-mm-2", dst, 0, body, true, sha256Hex(body), false)
	if fin.Bytes != int64(len(body)) {
		t.Fatalf("bytes = %d, want %d", fin.Bytes, len(body))
	}
}

func TestUploadFile_RejectsExistingWithoutOverwrite(t *testing.T) {
	cfg := mustCfg(t)
	dst := filepath.Join(cfg.LogDir, "present.bin")
	if err := os.WriteFile(dst, []byte("OLD"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	reg := NewRegistry(cfg, mustExec(t))
	_, err := reg.uploadFile(mustMarshal(map[string]any{
		"path":         dst,
		"upload_id":    "u-exist",
		"chunk":        base64.StdEncoding.EncodeToString([]byte("NEW")),
		"chunk_offset": 0,
		"chunk_done":   true,
	}))
	if !errors.Is(err, ErrFileExists) {
		t.Fatalf("expected ErrFileExists, got %v", err)
	}
	got, _ := os.ReadFile(dst)
	if string(got) != "OLD" {
		t.Fatalf("file changed: %q", got)
	}
}

func TestUploadFile_OverwriteTrueReplaces(t *testing.T) {
	cfg := mustCfg(t)
	dst := filepath.Join(cfg.LogDir, "replace.bin")
	if err := os.WriteFile(dst, []byte("OLD"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	reg := NewRegistry(cfg, mustExec(t))
	body := []byte("brand new contents")
	_, fin := uploadOnce(t, reg, "u-ov", dst, 0, body, true, sha256Hex(body), true)
	if !fin.Overwritten {
		t.Fatalf("overwritten flag should be true")
	}
	onDisk, _ := os.ReadFile(dst)
	if !bytes.Equal(onDisk, body) {
		t.Fatalf("readback = %q, want %q", onDisk, body)
	}
}

func TestUploadFile_RejectsDisallowedPath(t *testing.T) {
	cfg := mustCfg(t)
	reg := NewRegistry(cfg, mustExec(t))
	_, err := reg.uploadFile(mustMarshal(map[string]any{
		"path":         "C:\\Windows\\System32\\drivers\\etc\\hosts",
		"upload_id":    "u-bad",
		"chunk":        base64.StdEncoding.EncodeToString([]byte("x")),
		"chunk_offset": 0,
		"chunk_done":   true,
	}))
	if err == nil {
		t.Fatal("expected error for disallowed path")
	}
}

func TestUploadFile_RequiresFields(t *testing.T) {
	cfg := mustCfg(t)
	reg := NewRegistry(cfg, mustExec(t))
	cases := []map[string]any{
		{"upload_id": "x", "chunk": "YQ==", "chunk_offset": 0, "chunk_done": true}, // missing path
		{"path": "p", "chunk": "YQ==", "chunk_offset": 0, "chunk_done": true},      // missing upload_id
	}
	for i, p := range cases {
		_, err := reg.uploadFile(mustMarshal(p))
		if !errors.Is(err, ErrMissingField) {
			t.Fatalf("case %d: expected ErrMissingField, got %v", i, err)
		}
	}
}

func TestUploadFile_SHA256Mismatch_DoesNotPromote(t *testing.T) {
	cfg := mustCfg(t)
	reg := NewRegistry(cfg, mustExec(t))
	dst := filepath.Join(cfg.LogDir, "mismatch.bin")
	body := []byte("the real content")
	wrongHash := sha256Hex([]byte("something else"))

	_, err := reg.uploadFile(mustMarshal(map[string]any{
		"path":             dst,
		"upload_id":        "u-mis",
		"chunk":            base64.StdEncoding.EncodeToString(body),
		"chunk_offset":     0,
		"chunk_done":       true,
		"expected_sha256":  wrongHash,
	}))
	if err == nil {
		t.Fatal("expected sha256 mismatch error")
	}
	if !strings.Contains(err.Error(), "sha256 mismatch") {
		t.Fatalf("error = %v, want 'sha256 mismatch' substring", err)
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Fatalf("dst should not exist after mismatch: %v", err)
	}
	if _, err := os.Stat(dst + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf(".tmp should be removed after mismatch: %v", err)
	}
}

func TestUploadFile_NoExpectedSHA_StillVerifiedTrue(t *testing.T) {
	cfg := mustCfg(t)
	reg := NewRegistry(cfg, mustExec(t))
	dst := filepath.Join(cfg.LogDir, "nosha.bin")
	body := []byte("trust me bro")

	_, fin := uploadOnce(t, reg, "u-nosha", dst, 0, body, true, "", false)
	if !fin.Verified {
		t.Fatalf("verified should be true when no expected hash was provided")
	}
	if fin.SHA256 != sha256Hex(body) {
		t.Fatalf("sha = %s, want %s", fin.SHA256, sha256Hex(body))
	}
}

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
