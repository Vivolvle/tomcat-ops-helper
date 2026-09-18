package actions

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// uploadChunkSize is the canonical chunk size used by the server's
// /upload endpoint and documented for MCP clients. The helper accepts
// arbitrary chunk sizes — this constant only matters server-side.
const uploadChunkSize = 64 * 1024

// UploadParams is the JSON payload carried in Command.Params for the
// "upload_file" action. Each chunk arrives as a separate Command; the
// helper reconstructs the file from offset 0. All fields live inside
// Params (not the outer Envelope) so the wire format stays clean for
// every other message type.
type UploadParams struct {
	Path           string `json:"path"`            // absolute destination on the helper
	UploadID       string `json:"upload_id"`       // session id; client-supplied
	ChunkB64       string `json:"chunk,omitempty"` // base64 of this chunk's bytes
	ChunkOffset    int64  `json:"chunk_offset"`    // byte offset within the file
	ChunkDone      bool   `json:"chunk_done"`      // true on last chunk
	ExpectedSHA256 string `json:"expected_sha256,omitempty"`
	Overwrite      bool   `json:"overwrite,omitempty"`  // replace existing file (default false)
	QueryOnly      bool   `json:"query_only,omitempty"` // report resumable offset without writing
}

// UploadProgressData is the Result data sent back for intermediate
// (non-final) chunks. The caller can use it for progress reporting.
type UploadProgressData struct {
	UploadID     string `json:"upload_id"`
	BytesWritten int64  `json:"bytes_written"`
	NextOffset   int64  `json:"next_offset"`
	Accepted     bool   `json:"accepted"`
	SessionFound bool   `json:"session_found"`
}

// UploadResultData is the Result data sent back on the final chunk.
type UploadResultData struct {
	UploadID    string `json:"upload_id"`
	Path        string `json:"path"`
	Bytes       int64  `json:"bytes"`
	SHA256      string `json:"sha256"`
	Verified    bool   `json:"verified"`    // true when expected_sha256 matched (or none was given)
	Overwritten bool   `json:"overwritten"` // true if a pre-existing file was replaced
}

// uploadSession tracks one in-flight upload. Fields are protected by
// `mu` so the action is safe under concurrent calls (in practice the
// server sends one chunk at a time, but defending against reordering or
// duplicates is cheap).
type uploadSession struct {
	mu          sync.Mutex
	tmpPath     string
	finalPath   string
	f           *os.File
	hasher      hash.Hash
	written     int64
	expectedSHA string
	overwrite   bool
}

func (s *uploadSession) closeAndRemove() {
	if s.f != nil {
		_ = s.f.Close()
		s.f = nil
	}
	if s.tmpPath != "" {
		_ = os.Remove(s.tmpPath)
	}
}

type uploadRegistry struct {
	mu       sync.Mutex
	sessions map[string]*uploadSession
}

func newUploadRegistry() *uploadRegistry {
	return &uploadRegistry{sessions: make(map[string]*uploadSession)}
}

// cleanupStale removes sessions older than maxAge. Called periodically by
// the helper to avoid .tmp accumulation when server disconnects without
// sending cancel_upload.
func (u *uploadRegistry) cleanupStale(maxAge time.Duration) int {
	u.mu.Lock()
	defer u.mu.Unlock()
	now := time.Now()
	removed := 0
	for id, sess := range u.sessions {
		sess.mu.Lock()
		if sess.f != nil {
			if info, err := sess.f.Stat(); err == nil {
				if now.Sub(info.ModTime()) > maxAge {
					sess.closeAndRemove()
					delete(u.sessions, id)
					removed++
				}
			}
		}
		sess.mu.Unlock()
	}
	return removed
}

func (u *uploadRegistry) get(id string) (*uploadSession, bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	s, ok := u.sessions[id]
	return s, ok
}

func (u *uploadRegistry) put(id string, s *uploadSession) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.sessions[id] = s
}

// del removes the session and aborts any in-flight file (close + remove
// .tmp). Called on success after rename, on every error path, and on
// chunk-offset mismatch.
func (u *uploadRegistry) del(id string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if s, ok := u.sessions[id]; ok {
		s.closeAndRemove()
	}
	delete(u.sessions, id)
}

// cancelUpload aborts an in-flight upload and removes its .tmp file.
// Called by the server when a stall is detected or the operator cancels.
func (r *Registry) cancelUpload(params json.RawMessage) (json.RawMessage, error) {
	var p struct {
		UploadID string `json:"upload_id"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("cancel_upload: params: %w", err)
	}
	if p.UploadID == "" {
		return nil, fmt.Errorf("%w: upload_id", ErrMissingField)
	}
	sess, exists := r.uploads.get(p.UploadID)
	if !exists {
		// Already cleaned up or never existed — success either way.
		return mustMarshal(map[string]any{"cancelled": true, "upload_id": p.UploadID, "was_active": false}), nil
	}
	sess.mu.Lock()
	tmpPath := sess.tmpPath
	sess.mu.Unlock()
	r.uploads.del(p.UploadID)
	return mustMarshal(map[string]any{"cancelled": true, "upload_id": p.UploadID, "was_active": true, "tmp_removed": tmpPath}), nil
}

// uploadFile handles a single chunk of the upload_file action. The
// helper sees one Command per chunk and returns a Result for each;
// intermediate Results carry UploadProgressData and the final one
// carries UploadResultData.
//
// File lifecycle per upload:
//
//  1. chunk_offset=0 with a fresh upload_id   → open <path>.tmp
//  2. subsequent chunks                       → append, fsync, hash
//  3. chunk_done=true                         → close, verify sha256,
//     atomic rename → final path
//
// Offset and base64 validation errors preserve the session so callers can
// retry. Errors that may leave file/hash state uncertain abort and remove it.
func (r *Registry) uploadFile(params json.RawMessage) (json.RawMessage, error) {
	var p UploadParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("upload_file: params: %w", err)
	}
	if p.Path == "" {
		return nil, fmt.Errorf("%w: path", ErrMissingField)
	}
	if p.UploadID == "" {
		return nil, fmt.Errorf("%w: upload_id", ErrMissingField)
	}

	abs, err := filepath.Abs(p.Path)
	if err != nil {
		return nil, fmt.Errorf("upload_file: path: %w", err)
	}
	if !r.cfg.IsAllowedWorkDir(abs) {
		return nil, fmt.Errorf("upload_file: path not in allowed dir: %s; place the file under one of allowed_dirs: %s", abs, strings.Join(r.cfg.AllowedDirs, ", "))
	}

	sess, exists := r.uploads.get(p.UploadID)
	if p.QueryOnly {
		if !exists {
			return mustMarshal(UploadProgressData{UploadID: p.UploadID, SessionFound: false}), nil
		}
		sess.mu.Lock()
		defer sess.mu.Unlock()
		if sess.finalPath != abs {
			return nil, fmt.Errorf("upload_file: upload_id=%s belongs to %s, not %s", p.UploadID, sess.finalPath, abs)
		}
		return mustMarshal(UploadProgressData{UploadID: p.UploadID, BytesWritten: sess.written, NextOffset: sess.written, SessionFound: true}), nil
	}
	if !exists {
		// First chunk: validate metadata and open the session.
		if p.ChunkOffset != 0 {
			return nil, fmt.Errorf(
				"upload_file: chunk_offset=%d but no session for upload_id=%s",
				p.ChunkOffset, p.UploadID)
		}
		if !p.Overwrite {
			if _, err := os.Stat(abs); err == nil {
				return nil, fmt.Errorf("%w: %s", ErrFileExists, abs)
			}
		}
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			return nil, fmt.Errorf("upload_file: mkdir: %w", err)
		}
		tmp := abs + ".tmp"
		f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
		if err != nil {
			return nil, fmt.Errorf("upload_file: open tmp: %w", err)
		}
		sess = &uploadSession{
			tmpPath:     tmp,
			finalPath:   abs,
			f:           f,
			hasher:      sha256.New(),
			expectedSHA: p.ExpectedSHA256,
			overwrite:   p.Overwrite,
		}
		r.uploads.put(p.UploadID, sess)
	}

	// Decode + write this chunk. Holding the session mutex across the
	// whole chunk keeps offset/hash consistent.
	sess.mu.Lock()
	defer sess.mu.Unlock()

	if p.ChunkOffset != sess.written {
		// Preserve the session so callers can query/retry from expected.
		return nil, fmt.Errorf(
			"upload_file: chunk_offset=%d != expected=%d decoded file bytes; session preserved, retry at expected offset or use query_only=true",
			p.ChunkOffset, sess.written)
	}

	raw, err := base64.StdEncoding.DecodeString(p.ChunkB64)
	if err != nil {
		return nil, fmt.Errorf("upload_file: chunk base64 at decoded-byte offset %d (session preserved; retry this chunk): %w", sess.written, err)
	}
	if _, err := sess.f.Write(raw); err != nil {
		r.uploads.del(p.UploadID)
		return nil, fmt.Errorf("upload_file: write: %w", err)
	}
	sess.hasher.Write(raw)
	sess.written += int64(len(raw))

	if !p.ChunkDone {
		return mustMarshal(UploadProgressData{
			UploadID:     p.UploadID,
			BytesWritten: sess.written,
			NextOffset:   sess.written,
			Accepted:     true,
			SessionFound: true,
		}), nil
	}

	// Finalize: fsync, close, verify sha256, atomic rename.
	if err := sess.f.Sync(); err != nil {
		r.uploads.del(p.UploadID)
		return nil, fmt.Errorf("upload_file: sync: %w", err)
	}
	if err := sess.f.Close(); err != nil {
		r.uploads.del(p.UploadID)
		return nil, fmt.Errorf("upload_file: close: %w", err)
	}
	sess.f = nil

	actualSHA := hex.EncodeToString(sess.hasher.Sum(nil))
	verified := sess.expectedSHA == "" || sess.expectedSHA == actualSHA
	if !verified {
		_ = os.Remove(sess.tmpPath)
		r.uploads.del(p.UploadID)
		return nil, fmt.Errorf(
			"upload_file: sha256 mismatch: got %s want %s",
			actualSHA, sess.expectedSHA)
	}

	if err := os.Rename(sess.tmpPath, sess.finalPath); err != nil {
		r.uploads.del(p.UploadID)
		return nil, fmt.Errorf("upload_file: rename: %w", err)
	}

	// Success — drop the session but capture the data we still need
	// for the response.
	finalPath := sess.finalPath
	written := sess.written
	overwrite := sess.overwrite
	r.uploads.del(p.UploadID)

	return mustMarshal(UploadResultData{
		UploadID:    p.UploadID,
		Path:        finalPath,
		Bytes:       written,
		SHA256:      actualSHA,
		Verified:    verified,
		Overwritten: overwrite,
	}), nil
}
