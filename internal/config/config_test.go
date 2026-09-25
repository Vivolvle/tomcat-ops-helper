package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeTempConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func validJSON() string {
	return `{
		"server_url": "wss://ops.example.com/ws",
		"token": "abcdefghijklmnop",
		"tomcat_home": "C:\\tomcat",
		"jdk_home": "C:\\jdk",
		"log_dir": "C:\\tomcat\\logs",
		"allowed_dirs": ["C:\\tomcat","C:\\tomcat\\logs"],
		"command_timeout": 15,
		"max_output_size": 1048576,
		"reconnect_interval": 3
	}`
}

func TestLoadHelper_OK(t *testing.T) {
	path := writeTempConfig(t, validJSON())
	cfg, err := LoadHelper(path)
	if err != nil {
		t.Fatalf("LoadHelper: %v", err)
	}
	if cfg.CommandTimeout != 15*time.Second {
		t.Fatalf("command timeout = %v", cfg.CommandTimeout)
	}
	if cfg.ReconnectInterval != 3*time.Second {
		t.Fatalf("reconnect = %v", cfg.ReconnectInterval)
	}
	if cfg.MaxOutputSize != 1048576 {
		t.Fatalf("max output = %d", cfg.MaxOutputSize)
	}
	if cfg.LogFile != "helper.log" {
		t.Fatalf("default log file not applied: %s", cfg.LogFile)
	}
}

func TestLoadHelper_DurationStringAccepted(t *testing.T) {
	body := `{
		"server_url":"wss://x","token":"longenough",
		"tomcat_home":"C:\\t","jdk_home":"C:\\j","log_dir":"C:\\t\\l",
		"allowed_dirs":["C:\\t"],
		"command_timeout":"45s","max_output_size":"2M","reconnect_interval":"2s"
	}`
	cfg, err := LoadHelper(writeTempConfig(t, body))
	if err != nil {
		t.Fatalf("LoadHelper: %v", err)
	}
	if cfg.CommandTimeout != 45*time.Second {
		t.Fatalf("command timeout = %v", cfg.CommandTimeout)
	}
	if cfg.MaxOutputSize != 2<<20 {
		t.Fatalf("max output = %d", cfg.MaxOutputSize)
	}
	if cfg.ReconnectInterval != 2*time.Second {
		t.Fatalf("reconnect = %v", cfg.ReconnectInterval)
	}
}

func TestLoadHelper_AppliesDefaults(t *testing.T) {
	body := `{
		"server_url":"wss://x",
		"token":"longenough",
		"tomcat_home":"C:\\t","jdk_home":"C:\\j",
		"log_dir":"C:\\t\\l",
		"allowed_dirs":["C:\\t"]
	}`
	cfg, err := LoadHelper(writeTempConfig(t, body))
	if err != nil {
		t.Fatalf("LoadHelper: %v", err)
	}
	if cfg.CommandTimeout != 30*time.Second {
		t.Fatalf("default command timeout = %v", cfg.CommandTimeout)
	}
	if cfg.MaxOutputSize != 1<<20 {
		t.Fatalf("default max output = %d", cfg.MaxOutputSize)
	}
	if cfg.ReconnectInterval != 5*time.Second {
		t.Fatalf("default reconnect = %v", cfg.ReconnectInterval)
	}
}

func TestLoadHelper_RejectsBadURL(t *testing.T) {
	body := validJSON()
	body = `{"server_url":"http://x","token":"longenough","tomcat_home":"C:\\t","jdk_home":"C:\\j","log_dir":"C:\\t\\l","allowed_dirs":["C:\\t"]}`
	if _, err := LoadHelper(writeTempConfig(t, body)); err == nil {
		t.Fatal("expected error for http URL")
	}
}

func TestLoadHelper_RejectsShortToken(t *testing.T) {
	body := `{"server_url":"wss://x","token":"abc","tomcat_home":"C:\\t","jdk_home":"C:\\j","log_dir":"C:\\t\\l","allowed_dirs":["C:\\t"]}`
	if _, err := LoadHelper(writeTempConfig(t, body)); err == nil {
		t.Fatal("expected error for short token")
	}
}

func TestLoadHelper_RejectsEmptyAllowedDirs(t *testing.T) {
	body := `{"server_url":"wss://x","token":"longenough","tomcat_home":"C:\\t","jdk_home":"C:\\j","log_dir":"C:\\t\\l","allowed_dirs":[]}`
	if _, err := LoadHelper(writeTempConfig(t, body)); err == nil {
		t.Fatal("expected error for empty allowed_dirs")
	}
}

func TestLoadHelper_NegativeTimeoutFallsBackToDefault(t *testing.T) {
	body := `{"server_url":"wss://x","token":"longenough","tomcat_home":"C:\\t","jdk_home":"C:\\j","log_dir":"C:\\t\\l","allowed_dirs":["C:\\t"],"command_timeout":-1}`
	cfg, err := LoadHelper(writeTempConfig(t, body))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.CommandTimeout != 30*time.Second {
		t.Fatalf("expected fallback to 30s, got %v", cfg.CommandTimeout)
	}
}

func TestLoadHelper_MissingFile(t *testing.T) {
	if _, err := LoadHelper("does-not-exist.json"); err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestLoadHelper_DBConfig_OptionalAndValidated(t *testing.T) {
	body := `{
		"server_url":"wss://x","token":"longenough",
		"tomcat_home":"C:\\t","jdk_home":"C:\\j","log_dir":"C:\\t\\l","allowed_dirs":["C:\\t"],
		"esb":{"host":"db1","port":3306,"user":"u","password":"p","database":"esb"},
		"erp":{"type":"oracle","host":"db2","port":1521,"user":"u","password":"p","database":"ORCL"}
	}`
	cfg, err := LoadHelper(writeTempConfig(t, body))
	if err != nil {
		t.Fatalf("LoadHelper: %v", err)
	}
	if cfg.ESB == nil || cfg.ESB.Driver() != "mysql" || cfg.ESB.EffectivePort("mysql") != 3306 {
		t.Fatalf("esb bad: %+v", cfg.ESB)
	}
	if cfg.ERP == nil || cfg.ERP.Driver() != "oracle" || cfg.ERP.EffectivePort("oracle") != 1521 {
		t.Fatalf("erp bad: %+v", cfg.ERP)
	}
}

func TestLoadHelper_DBConfig_DefaultPortForSQLServer(t *testing.T) {
	body := `{
		"server_url":"wss://x","token":"longenough",
		"tomcat_home":"C:\\t","jdk_home":"C:\\j","log_dir":"C:\\t\\l","allowed_dirs":["C:\\t"],
		"erp":{"type":"sqlserver","host":"db","user":"u","password":"p","database":"erp"}
	}`
	cfg, err := LoadHelper(writeTempConfig(t, body))
	if err != nil {
		t.Fatalf("LoadHelper: %v", err)
	}
	if cfg.ERP.EffectivePort("sqlserver") != 1433 {
		t.Fatalf("expected 1433, got %d", cfg.ERP.EffectivePort("sqlserver"))
	}
}

func TestLoadHelper_DBConfig_MissingFields(t *testing.T) {
	body := `{
		"server_url":"wss://x","token":"longenough",
		"tomcat_home":"C:\\t","jdk_home":"C:\\j","log_dir":"C:\\t\\l","allowed_dirs":["C:\\t"],
		"esb":{"host":"","user":"u","password":"p","database":"esb"}
	}`
	if _, err := LoadHelper(writeTempConfig(t, body)); err == nil {
		t.Fatal("expected error for missing esb host")
	}
}

func TestLoadHelper_DBConfig_BadType(t *testing.T) {
	body := `{
		"server_url":"wss://x","token":"longenough",
		"tomcat_home":"C:\\t","jdk_home":"C:\\j","log_dir":"C:\\t\\l","allowed_dirs":["C:\\t"],
		"erp":{"type":"postgres","host":"db","user":"u","password":"p","database":"erp"}
	}`
	if _, err := LoadHelper(writeTempConfig(t, body)); err == nil {
		t.Fatal("expected error for unsupported db type")
	}
}

func TestLoadHelper_DBConfig_NotConfiguredIsFine(t *testing.T) {
	body := validJSON() // no esb / erp blocks
	cfg, err := LoadHelper(writeTempConfig(t, body))
	if err != nil {
		t.Fatalf("LoadHelper: %v", err)
	}
	if cfg.ESB != nil || cfg.ERP != nil {
		t.Fatalf("expected nil connectors, got esb=%+v erp=%+v", cfg.ESB, cfg.ERP)
	}
}

// TestLoadHelper_DBConfig_Enabled covers the three states of the
// enabled flag: unset (default true), explicit true, explicit false.
// The field is a *bool so an absent key and "false" are distinguishable
// in code but collapse to the same end-user intent.
func TestLoadHelper_DBConfig_Enabled(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		enabled bool
	}{
		{
			name: "unset defaults to true",
			body: `{
				"server_url":"wss://x","token":"longenough",
				"tomcat_home":"C:\\t","jdk_home":"C:\\j","log_dir":"C:\\t\\l","allowed_dirs":["C:\\t"],
				"esb":{"host":"db","user":"u","password":"p","database":"d"}
			}`,
			enabled: true,
		},
		{
			name: "explicit true",
			body: `{
				"server_url":"wss://x","token":"longenough",
				"tomcat_home":"C:\\t","jdk_home":"C:\\j","log_dir":"C:\\t\\l","allowed_dirs":["C:\\t"],
				"esb":{"host":"db","user":"u","password":"p","database":"d","enabled":true}
			}`,
			enabled: true,
		},
		{
			name: "explicit false",
			body: `{
				"server_url":"wss://x","token":"longenough",
				"tomcat_home":"C:\\t","jdk_home":"C:\\j","log_dir":"C:\\t\\l","allowed_dirs":["C:\\t"],
				"erp":{"type":"oracle","host":"db","user":"u","password":"p","database":"d","enabled":false}
			}`,
			enabled: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := LoadHelper(writeTempConfig(t, tc.body))
			if err != nil {
				t.Fatalf("LoadHelper: %v", err)
			}
			var got bool
			if cfg.ESB != nil {
				got = cfg.ESB.IsEnabled()
			} else {
				got = cfg.ERP.IsEnabled()
			}
			if got != tc.enabled {
				t.Fatalf("IsEnabled=%v, want %v", got, tc.enabled)
			}
		})
	}
}

// TestLoadHelper_DBConfig_ForbidCmd covers the read-only lock flag:
// absent → false (writes allowed, nothing to maintain), explicit true
// on either connector → surfaced on DBConfig for the dbop layer.
func TestLoadHelper_DBConfig_ForbidCmd(t *testing.T) {
	base := `{
		"server_url":"wss://x","token":"longenough",
		"tomcat_home":"C:\\t","jdk_home":"C:\\j","log_dir":"C:\\t\\l","allowed_dirs":["C:\\t"],
		%s
	}`
	cases := []struct {
		name string
		body string
		esb  bool
		erp  bool
	}{
		{
			name: "unset defaults to false on both",
			body: `"esb":{"host":"db","user":"u","password":"p","database":"d"},
				"erp":{"type":"oracle","host":"db","user":"u","password":"p","database":"d"}`,
		},
		{
			name: "esb locked, erp not",
			body: `"esb":{"host":"db","user":"u","password":"p","database":"d","forbid_cmd":true},
				"erp":{"type":"oracle","host":"db","user":"u","password":"p","database":"d"}`,
			esb: true,
		},
		{
			name: "erp locked, esb not",
			body: `"esb":{"host":"db","user":"u","password":"p","database":"d"},
				"erp":{"type":"oracle","host":"db","user":"u","password":"p","database":"d","forbid_cmd":true}`,
			erp: true,
		},
		{
			name: "explicit false stays false",
			body: `"esb":{"host":"db","user":"u","password":"p","database":"d","forbid_cmd":false},
				"erp":{"type":"oracle","host":"db","user":"u","password":"p","database":"d","forbid_cmd":false}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := LoadHelper(writeTempConfig(t, fmt.Sprintf(base, tc.body)))
			if err != nil {
				t.Fatalf("LoadHelper: %v", err)
			}
			if cfg.ESB.ForbidCmd != tc.esb {
				t.Fatalf("esb ForbidCmd=%v, want %v", cfg.ESB.ForbidCmd, tc.esb)
			}
			if cfg.ERP.ForbidCmd != tc.erp {
				t.Fatalf("erp ForbidCmd=%v, want %v", cfg.ERP.ForbidCmd, tc.erp)
			}
		})
	}
}

// TestLoadHelper_SetsLoadedAt covers the "did helper pick up my edit?"
// check exposed via status.config_loaded_at and validate_config.
// Without this stamp, an LLM can only guess why a config change didn't
// take effect.
func TestLoadHelper_SetsLoadedAt(t *testing.T) {
	before := time.Now()
	cfg, err := LoadHelper(writeTempConfig(t, validJSON()))
	if err != nil {
		t.Fatalf("LoadHelper: %v", err)
	}
	if cfg.LoadedAt.Before(before) {
		t.Fatalf("LoadedAt (%s) is before test start (%s)", cfg.LoadedAt, before)
	}
	if d := time.Since(cfg.LoadedAt); d > 5*time.Second {
		t.Fatalf("LoadedAt not close to now: %s", d)
	}
}

// TestHelperConfig_ServiceNameOptional confirms service_name is purely an
// override (empty is fine; regen_service derives the name from server.xml).
func TestHelperConfig_ServiceNameOptional(t *testing.T) {
	cfg, err := LoadHelper(writeTempConfig(t, validJSON()))
	if err != nil {
		t.Fatalf("LoadHelper: %v", err)
	}
	if cfg.ServiceName != "" {
		t.Fatalf("expected empty default service_name, got %q", cfg.ServiceName)
	}
}

func TestLoadHelper_ServiceNameOverride(t *testing.T) {
	body := `{
		"server_url":"wss://x","token":"longenough",
		"tomcat_home":"C:\\t","jdk_home":"C:\\j","log_dir":"C:\\t\\l",
		"allowed_dirs":["C:\\t"],
		"service_name":"MyTomcat"
	}`
	cfg, err := LoadHelper(writeTempConfig(t, body))
	if err != nil {
		t.Fatalf("LoadHelper: %v", err)
	}
	if cfg.ServiceName != "MyTomcat" {
		t.Fatalf("ServiceName = %q, want MyTomcat", cfg.ServiceName)
	}
}

func TestHelperConfig_IsAllowedWorkDir(t *testing.T) {
	dir := t.TempDir()
	inner := filepath.Join(dir, "inner")
	if err := os.MkdirAll(inner, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	cfg := &HelperConfig{AllowedDirs: []string{dir}}
	if !cfg.IsAllowedWorkDir(inner) {
		t.Fatal("inner should be allowed")
	}
	if !cfg.IsAllowedWorkDir(dir) {
		t.Fatal("root should be allowed")
	}
	if cfg.IsAllowedWorkDir(filepath.Join(dir, "..", "elsewhere")) {
		t.Fatal("outside path should not be allowed")
	}
	if cfg.IsAllowedWorkDir("") {
		t.Fatal("empty should not be allowed")
	}
}

func TestLoadServer_OK(t *testing.T) {
	body := `{
		"listen":":8443",
		"tls_cert":"/tmp/cert.pem",
		"tls_key":"/tmp/key.pem",
		"tokens":["longenough1","longenough2"],
		"audit_log":"/tmp/audit.log",
		"agent_listen":"127.0.0.1:8080"
	}`
	cfg, err := LoadServer(writeTempConfig(t, body))
	if err != nil {
		t.Fatalf("LoadServer: %v", err)
	}
	if cfg.Listen != ":8443" {
		t.Fatalf("listen = %s", cfg.Listen)
	}
	if !cfg.HasToken("longenough1") {
		t.Fatal("token missing")
	}
	if cfg.HasToken("nope") {
		t.Fatal("unexpected token match")
	}
}

func TestLoadServer_Defaults(t *testing.T) {
	body := `{"tls_cert":"/c","tls_key":"/k","tokens":["longenough1"]}`
	cfg, err := LoadServer(writeTempConfig(t, body))
	if err != nil {
		t.Fatalf("LoadServer: %v", err)
	}
	if cfg.Listen != ":443" {
		t.Fatalf("default listen = %s", cfg.Listen)
	}
	if cfg.AgentListen != "127.0.0.1:7777" {
		t.Fatalf("default agent listen = %s", cfg.AgentListen)
	}
	if cfg.AuditLog != "audit.log" {
		t.Fatalf("default audit log = %s", cfg.AuditLog)
	}
	if !strings.HasSuffix(cfg.ProblemLog, "problem.md") {
		t.Fatalf("default problem log = %s", cfg.ProblemLog)
	}
}

func TestLoadServer_ProblemLog(t *testing.T) {
	body := `{
		"tls_cert":"/c","tls_key":"/k","tokens":["longenough1"],
		"problem_log":"D:\\ops\\problems.md"
	}`
	cfg, err := LoadServer(writeTempConfig(t, body))
	if err != nil {
		t.Fatalf("LoadServer: %v", err)
	}
	if cfg.ProblemLog != `D:\ops\problems.md` {
		t.Fatalf("problem log = %s", cfg.ProblemLog)
	}
}

func TestLoadServer_RejectsMissingTLS(t *testing.T) {
	body := `{"tokens":["longenough1"]}`
	if _, err := LoadServer(writeTempConfig(t, body)); err == nil {
		t.Fatal("expected error for missing TLS")
	}
}

func TestLoadServer_RejectsShortToken(t *testing.T) {
	body := `{"tls_cert":"/c","tls_key":"/k","tokens":["abc"]}`
	if _, err := LoadServer(writeTempConfig(t, body)); err == nil {
		t.Fatal("expected error for short token")
	}
}

func TestLoadServer_WSListen(t *testing.T) {
	body := `{
		"listen":":8443",
		"ws_listen":"127.0.0.1:8444",
		"tls_cert":"/c","tls_key":"/k",
		"tokens":["longenough1"]
	}`
	cfg, err := LoadServer(writeTempConfig(t, body))
	if err != nil {
		t.Fatalf("LoadServer: %v", err)
	}
	if cfg.WSListen != "127.0.0.1:8444" {
		t.Fatalf("ws_listen = %q", cfg.WSListen)
	}
}

func TestLoadServer_WSListen_DefaultsEmpty(t *testing.T) {
	body := `{"tls_cert":"/c","tls_key":"/k","tokens":["longenough1"]}`
	cfg, err := LoadServer(writeTempConfig(t, body))
	if err != nil {
		t.Fatalf("LoadServer: %v", err)
	}
	if cfg.WSListen != "" {
		t.Fatalf("ws_listen default = %q, want empty", cfg.WSListen)
	}
}
