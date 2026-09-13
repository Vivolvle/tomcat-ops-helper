package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"tomcat-ops-helper/internal/protocol"
	"tomcat-ops-helper/internal/wsserver"
)

// TestUploadEndToEnd wires a real wsserver, connects a fake helper,
// POSTs a multi-chunk file to /upload, and asserts that the helper
// receives a sequence of upload_file commands whose chunks reassemble
// into the original bytes, plus that the HTTP response surfaces the
// final Result.
func TestUploadEndToEnd(t *testing.T) {
	relay := newTestRelay(t)

	var (
		mu        sync.Mutex
		gotChunks []protocol.Command
	)

	wsHandler := wsserver.New(relay.auth, func(*wsserver.Client) {}).
		WithHello(func(c *wsserver.Client) { relay.register(c) }).
		WithResultHandler(func(c *wsserver.Client, r protocol.Result) { relay.deliver(c, r) }).
		WithToolList(func(c *wsserver.Client, tl protocol.ToolList) { relay.storeTools(c, tl) }).
		WithDisconnect(func(c *wsserver.Client) { relay.unregister(c) })

	mux := http.NewServeMux()
	mux.Handle("/ws", wsHandler)
	mux.HandleFunc("/upload", relay.handleUpload)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	u, _ := url.Parse(srv.URL)
	u.Scheme = "ws"
	u.Path = "/ws"
	wsURL := u.String()

	conn, _, err := websocket.DefaultDialer.Dial(wsURL, http.Header{
		"Authorization": []string{"Bearer tok-1234567890"},
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// Fake helper handshake.
	if err := conn.WriteJSON((protocol.Hello{
		ClientID: "test-helper", Token: "tok-1234567890",
	}).ToEnvelope()); err != nil {
		t.Fatalf("hello: %v", err)
	}

	// Pump helper-side: read every Command, capture it, echo back a
	// Result that matches the action (progress for non-final chunks,
	// a final UploadResultData on the last one).
	finalData := []byte(`{"upload_id":"X","path":"Y","bytes":0,"sha256":"","verified":true,"overwritten":false}`)
	go func() {
		for {
			_, raw, err := conn.ReadMessage()
			if err != nil {
				return
			}
			env, _ := protocol.DecodeEnvelope(raw)
			if env.Type != protocol.TypeCommand {
				continue
			}
			cmd, _ := protocol.CommandFromEnvelope(env)
			mu.Lock()
			gotChunks = append(gotChunks, cmd)
			mu.Unlock()

			// Synthesize a Result; the test asserts bytes_written
			// travels back to the operator only via the final chunk's
			// raw data, so we just need a well-formed envelope.
			var p struct {
				ChunkDone bool   `json:"chunk_done"`
				UploadID  string `json:"upload_id"`
				Path      string `json:"path"`
			}
			_ = json.Unmarshal(cmd.Params, &p)
			data := json.RawMessage(`{"upload_id":"X","accepted":true,"bytes_written":0}`)
			if p.ChunkDone {
				data = finalData
			}
			_ = conn.WriteJSON((protocol.Result{ID: cmd.ID, Success: true, Data: data}).ToEnvelope())
		}
	}()

	// Wait for register before POSTing so client_id resolves.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		relay.mu.Lock()
		_, ok := relay.clients["test-helper"]
		relay.mu.Unlock()
		if ok {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Build a payload large enough to force multiple chunks at the
	// 64 KB chunk size (3 chunks of ~70 KB each → 3 Commands).
	body := bytes.Repeat([]byte("XYZ"), 70*1024/3+1)
	wantSHA := sha256Hex(body)

	var form bytes.Buffer
	mw := multipart.NewWriter(&form)
	if err := mw.WriteField("path", "E:\\tomcat-helper\\app.jar"); err != nil {
		t.Fatalf("write path: %v", err)
	}
	if err := mw.WriteField("sha256", wantSHA); err != nil {
		t.Fatalf("write sha: %v", err)
	}
	if err := mw.WriteField("client_id", "test-helper"); err != nil {
		t.Fatalf("write client_id: %v", err)
	}
	fw, err := mw.CreateFormFile("file", "app.jar")
	if err != nil {
		t.Fatalf("create file: %v", err)
	}
	if _, err := fw.Write(body); err != nil {
		t.Fatalf("write body: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close mw: %v", err)
	}

	resp, err := http.Post(srv.URL+"/upload", mw.FormDataContentType(), &form)
	if err != nil {
		t.Fatalf("post upload: %v", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("upload status = %d, body=%s", resp.StatusCode, respBody)
	}
	t.Logf("upload response: %s", respBody)

	// Give the read pump a beat to drain the last chunk off the wire.
	time.Sleep(100 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if len(gotChunks) < 2 {
		t.Fatalf("expected multiple chunks, got %d", len(gotChunks))
	}

	// Reassemble bytes and check the chunks are contiguous.
	var reassembled bytes.Buffer
	var uploadID string
	for i, c := range gotChunks {
		var p struct {
			UploadID    string `json:"upload_id"`
			ChunkB64    string `json:"chunk"`
			ChunkOffset int64  `json:"chunk_offset"`
			ChunkDone   bool   `json:"chunk_done"`
		}
		if err := json.Unmarshal(c.Params, &p); err != nil {
			t.Fatalf("chunk %d decode: %v", i, err)
		}
		if p.UploadID == "" {
			t.Fatalf("chunk %d missing upload_id", i)
		}
		if i == 0 {
			uploadID = p.UploadID
		} else if p.UploadID != uploadID {
			t.Fatalf("upload_id changed mid-stream: %s vs %s", p.UploadID, uploadID)
		}
		if p.ChunkOffset != int64(reassembled.Len()) {
			t.Fatalf("chunk %d offset=%d, expected=%d", i, p.ChunkOffset, reassembled.Len())
		}
		raw, err := base64.StdEncoding.DecodeString(p.ChunkB64)
		if err != nil {
			t.Fatalf("chunk %d b64: %v", i, err)
		}
		reassembled.Write(raw)
		if i == len(gotChunks)-1 && !p.ChunkDone {
			t.Fatalf("last chunk chunk_done should be true")
		}
		if i < len(gotChunks)-1 && p.ChunkDone {
			t.Fatalf("non-last chunk chunk_done should be false")
		}
	}
	if !bytes.Equal(reassembled.Bytes(), body) {
		t.Fatalf("reassembled bytes mismatch: got %d, want %d", reassembled.Len(), len(body))
	}

	// Response should reference the path and bytes.
	var respJSON struct {
		Path  string `json:"path"`
		Bytes int64  `json:"bytes"`
	}
	if err := json.Unmarshal(respBody, &respJSON); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !strings.HasSuffix(respJSON.Path, "app.jar") {
		t.Fatalf("path = %q", respJSON.Path)
	}
	if respJSON.Bytes != int64(len(body)) {
		t.Fatalf("bytes = %d, want %d", respJSON.Bytes, len(body))
	}
}

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
