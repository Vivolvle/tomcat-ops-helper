package helpergui

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func guiPost(t *testing.T, url string, body any) *http.Response {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(guiHeader, "helper-gui")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func guiGet(t *testing.T, url string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	req.Header.Set(guiHeader, "helper-gui")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func newTestServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	dir := t.TempDir()
	ts := httptest.NewServer(New(filepath.Join(dir, "config.json"), filepath.Join(dir, "helper.exe")).Handler())
	t.Cleanup(ts.Close)
	return ts, dir
}

// TestAPIRequiresGUIHeader: without the marker header every /api route
// is rejected (local-CSRF guard).
func TestAPIRequiresGUIHeader(t *testing.T) {
	ts, _ := newTestServer(t)
	resp, err := http.Post(ts.URL+"/api/proc/start", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
}

// TestGetConfigTemplate: missing config.json → exists:false + defaults.
func TestGetConfigTemplate(t *testing.T) {
	ts, _ := newTestServer(t)
	resp := guiGet(t, ts.URL+"/api/config")
	defer resp.Body.Close()
	var got struct {
		OK     bool           `json:"ok"`
		Exists bool           `json:"exists"`
		Doc    map[string]any `json:"doc"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&got)
	if !got.OK || got.Exists {
		t.Fatalf("expected ok+missing, got %+v", got)
	}
	if got.Doc["log_file"] != "helper.log" || got.Doc["command_timeout"] != float64(30) {
		t.Fatalf("defaults wrong: log_file=%v command_timeout=%v", got.Doc["log_file"], got.Doc["command_timeout"])
	}
}

// TestDirList: files excluded, dirs sorted, parent computed; drive
// roots only asserted on Windows.
func TestDirList(t *testing.T) {
	dir := t.TempDir()
	for _, d := range []string{"b-dir", "a-dir"} {
		if err := os.Mkdir(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	parent, entries, err := ListDirs(dir)
	if err != nil {
		t.Fatal(err)
	}
	if parent == "" || parent == dir {
		t.Fatalf("parent = %q, want the temp parent", parent)
	}
	if len(entries) != 2 || entries[0].Name != "a-dir" || entries[1].Name != "b-dir" {
		t.Fatalf("entries = %+v", entries)
	}

	if runtime.GOOS == "windows" {
		drives, err := ListDrives()
		if err != nil {
			t.Fatal(err)
		}
		if len(drives) == 0 {
			t.Fatal("no drives enumerated on windows")
		}
		for _, d := range drives {
			if len(d) != 3 || d[1] != ':' || d[2] != '\\' {
				t.Fatalf("drive not C:\\-shaped: %q", d)
			}
		}
		// drive root has no parent
		if p, _, err := ListDirs(drives[0]); err != nil || p != "" {
			t.Fatalf("drive root parent = %q err=%v, want empty", p, err)
		}
	}
}

// TestFsDirsRoute: empty path lists drives; a bad path errors.
func TestFsDirsRoute(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("drive listing is windows-only")
	}
	ts, _ := newTestServer(t)
	resp := guiGet(t, ts.URL+"/api/fs/dirs")
	defer resp.Body.Close()
	var got struct {
		OK      bool           `json:"ok"`
		Entries []DirEntryInfo `json:"entries"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&got)
	if !got.OK || len(got.Entries) == 0 {
		t.Fatalf("drive listing failed: %+v", got)
	}

	resp2 := guiGet(t, ts.URL+"/api/fs/dirs?path="+filepath.Join(t.TempDir(), "nope"))
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing dir should 400, got %d", resp2.StatusCode)
	}
}
