package dbop

import (
	"archive/zip"
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/encoding/simplifiedchinese"
)

const maxFormBytes = 8 << 20
const maxFormExpanded = 32 << 20

// ReadFormEntity uses a fixed, bound SELECT and never passes BLOBs through JSON.
// It is safe on forbid_cmd connectors: no caller-controlled SQL is accepted.
func (o *SQLOp) ReadFormEntity(ctx context.Context, formID string) ([]byte, error) {
	if o.driver != "oracle" {
		return nil, errors.New("form_content currently supports Oracle FORMDOC only")
	}
	if len(formID) != 16 || strings.TrimSpace(formID) != formID {
		return nil, errors.New("form_id must contain exactly 16 bytes")
	}
	read := func() ([]byte, error) {
		pool, err := o.dbHandle(ctx)
		if err != nil {
			return nil, err
		}
		var size int64
		var data []byte
		err = pool.QueryRowContext(ctx, `SELECT NVL(DBMS_LOB.GETLENGTH(FORMENTITY),0), CASE WHEN DBMS_LOB.GETLENGTH(FORMENTITY) <= 8388608 THEN FORMENTITY ELSE NULL END FROM FORMDOC WHERE FORMID = :1`, formID).Scan(&size, &data)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errors.New("form not found")
		}
		if err != nil {
			return nil, fmt.Errorf("read FORMDOC: %w", err)
		}
		if size <= 0 {
			return nil, errors.New("FORMENTITY is empty")
		}
		if size > maxFormBytes {
			return nil, errors.New("FORMENTITY exceeds 8 MiB limit")
		}
		if int64(len(data)) != size {
			return nil, errors.New("BLOB length mismatch")
		}
		return data, ctx.Err()
	}
	data, err := read()
	if isRetryable(err) {
		_ = o.Close()
		data, err = read()
	}
	if err != nil {
		o.mu.Lock()
		o.markLocked(err)
		o.mu.Unlock()
	}
	return data, err
}

type FormFile struct {
	Path            string `json:"path"`
	Bytes           uint64 `json:"bytes"`
	CompressedBytes uint64 `json:"compressed_bytes"`
	Directory       bool   `json:"directory,omitempty"`
}
type FormContent struct {
	FormID       string     `json:"form_id"`
	ArchiveBytes int        `json:"archive_bytes"`
	Prefix       string     `json:"prefix"`
	Files        []FormFile `json:"files"`
	Path         string     `json:"path,omitempty"`
	Encoding     string     `json:"encoding,omitempty"`
	Text         string     `json:"text,omitempty"`
	TotalChars   int        `json:"total_chars,omitempty"`
	Offset       int        `json:"offset"`
	NextOffset   *int       `json:"next_offset,omitempty"`
	Truncated    bool       `json:"truncated"`
	Warnings     []string   `json:"warnings,omitempty"`
}

// InspectForm only reads archive members into bounded memory; it never extracts
// paths to disk, executes code, or recursively decodes nested payloads.
func InspectForm(ctx context.Context, data []byte, path, encoding string, offset, limit int) (FormContent, error) {
	out := FormContent{ArchiveBytes: len(data), Offset: offset}
	if len(data) > maxFormBytes {
		return out, errors.New("archive exceeds 8 MiB limit")
	}
	if offset < 0 || limit < 1 || limit > 64000 {
		return out, errors.New("offset must be nonnegative; max_chars must be between 1 and 64000")
	}
	if encoding != "auto" && encoding != "utf-8" && encoding != "gb18030" {
		return out, errors.New("encoding must be auto, utf-8, or gb18030")
	}
	payload := data
	if bytes.HasPrefix(data, []byte("MJU")) {
		out.Prefix = "MJU"
		payload = data[3:]
	}
	if !bytes.HasPrefix(payload, []byte("PK\x03\x04")) && !bytes.HasPrefix(payload, []byte("PK\x05\x06")) {
		return out, errors.New("expected ZIP or MJU-prefixed ZIP")
	}
	archive, err := zip.NewReader(bytes.NewReader(payload), int64(len(payload)))
	if err != nil {
		return out, fmt.Errorf("invalid ZIP: %w", err)
	}
	if len(archive.File) > 512 {
		return out, errors.New("archive exceeds 512 file limit")
	}
	if err := ctx.Err(); err != nil {
		return out, err
	}
	var selected *zip.File
	seen := map[string]bool{}
	var expanded uint64
	for _, f := range archive.File {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		if f.UncompressedSize64 > maxFormExpanded-expanded {
			return out, errors.New("archive exceeds 32 MiB expanded limit")
		}
		if seen[f.Name] {
			return out, errors.New("duplicate member path")
		}
		seen[f.Name] = true
		if f.Flags&1 != 0 {
			return out, errors.New("encrypted ZIP member unsupported")
		}
		if f.Method != zip.Store && f.Method != zip.Deflate {
			return out, errors.New("unsupported ZIP compression method")
		}
		expanded += f.UncompressedSize64
		out.Files = append(out.Files, FormFile{f.Name, f.UncompressedSize64, f.CompressedSize64, f.FileInfo().IsDir()})
		if f.Name == path {
			if selected != nil {
				return out, errors.New("duplicate member path")
			}
			selected = f
		}
	}
	if path == "" {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		return out, nil
	}
	if selected == nil || selected.FileInfo().IsDir() {
		return out, errors.New("file path not found or is a directory")
	}
	reader, err := selected.Open()
	if err != nil {
		return out, err
	}
	defer reader.Close()
	var buf bytes.Buffer
	scratch := make([]byte, 32*1024)
	for {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		n, e := reader.Read(scratch)
		if buf.Len()+n > maxFormExpanded {
			return out, errors.New("expanded file exceeds limit")
		}
		buf.Write(scratch[:n])
		if e == io.EOF {
			break
		}
		if e != nil {
			return out, fmt.Errorf("read ZIP member: %w", e)
		}
	}
	raw := buf.Bytes()
	if bytes.Contains(raw, []byte{0}) {
		return out, errors.New("member appears binary; text output only")
	}
	actual := encoding
	if actual == "auto" {
		actual = "utf-8"
		if !utf8.Valid(raw) {
			actual = "gb18030"
		}
	}
	if actual == "gb18030" {
		raw, err = simplifiedchinese.GB18030.NewDecoder().Bytes(raw)
		if err != nil {
			return out, err
		}
	}
	if !utf8.Valid(raw) {
		return out, errors.New("invalid text encoding")
	}
	runes := []rune(string(raw))
	out.TotalChars = len(runes)
	if offset > len(runes) {
		return out, errors.New("offset exceeds text length")
	}
	end := len(runes)
	if limit < len(runes)-offset {
		end = offset + limit
	}
	if end > len(runes) {
		end = len(runes)
	}
	out.Path = path
	out.Encoding = actual
	out.Text = string(runes[offset:end])
	out.Truncated = end < len(runes)
	if out.Truncated {
		out.NextOffset = &end
	}
	if strings.HasSuffix(strings.ToLower(path), "protected.js") {
		out.Warnings = []string{"This member may contain protected/encoded code. ZIP extraction does not decrypt or restore protected implementations."}
	}
	if err := ctx.Err(); err != nil {
		return out, err
	}
	return out, nil
}
