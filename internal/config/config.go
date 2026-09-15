// Package config loads and validates helper/server JSON configuration.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// HelperConfig mirrors the config.json shipped next to helper.exe.
type HelperConfig struct {
	ServerURL         string        `json:"server_url"`
	Token             string        `json:"token"`
	ClientID          string        `json:"client_id,omitempty"`
	TomcatHome        string        `json:"tomcat_home"`
	JDKHome           string        `json:"jdk_home"`
	LogDir            string        `json:"log_dir"`
	AllowedDirs       []string      `json:"allowed_dirs"`
	CommandTimeout    time.Duration `json:"command_timeout"`
	MaxOutputSize     int64         `json:"max_output_size"`
	ReconnectInterval time.Duration `json:"reconnect_interval"`
	HeartbeatInterval time.Duration `json:"heartbeat_interval"`
	HeartbeatTimeout  time.Duration `json:"heartbeat_timeout"`
	LogFile           string        `json:"log_file"`
	InsecureSkipVerify bool         `json:"insecure_skip_verify"`

	// LoadedAt is when this HelperConfig was parsed from disk. Set by
	// LoadHelper. Exposed (json:"-") so status / validate_config can
	// tell the operator which config snapshot helper is currently
	// running — useful when config.json was edited but helper not
	// restarted.
	LoadedAt time.Time `json:"-"`

	// ServiceName is the Windows service name registered for this
	// Tomcat (regen_service writes service.bat; tomcat10w.exe must be
	// renamed to <ServiceName>w.exe for the GUI monitor to bind).
	// Empty means "derive from server.xml <Connector port="N"/> (first
	// hit) and fall back to "Tomcat" if nothing is found."
	ServiceName string `json:"service_name,omitempty"`

	// ESB connector (always MySQL 8). Optional; nil means the esb_db_operator
	// tool is not advertised.
	ESB *DBConfig `json:"esb,omitempty"`
	// ERP connector. Optional. Type picks the driver — "oracle" or
	// "sqlserver". Nil means the erp_db_operator tool is not advertised.
	ERP *DBConfig `json:"erp,omitempty"`
}

// DBConfig is one database connector. Driver is implicit for ESB (mysql)
// and explicit for ERP ("oracle" | "sqlserver"). Port defaults are
// driver-specific when zero.
type DBConfig struct {
	Type     string `json:"type,omitempty"` // empty for ESB; "oracle" | "sqlserver" for ERP
	Host     string `json:"host"`
	Port     int    `json:"port"`
	User     string `json:"user"`
	Password string `json:"password"`
	Database string `json:"database"`
}

// Driver returns the lowercase driver name. Used by the dbop package to
// pick a DSN builder and by audit logs to tag which connector a query hit.
func (c *DBConfig) Driver() string {
	if c == nil {
		return ""
	}
	if c.Type != "" {
		return strings.ToLower(c.Type)
	}
	return "mysql"
}

// DefaultPort returns the conventional port when c.Port is zero. The caller
// has already determined the driver name via Driver().
func (c *DBConfig) DefaultPort(driver string) int {
	switch driver {
	case "mysql":
		return 3306
	case "sqlserver", "mssql":
		return 1433
	case "oracle":
		return 1521
	}
	return 0
}

// Validate returns an error when the config is structurally incomplete.
// Empty Type is allowed (treated as mysql); the caller resolves defaults.
func (c *DBConfig) Validate() error {
	if c == nil {
		return nil
	}
	if c.Host == "" {
		return errors.New("db host is required")
	}
	if c.User == "" {
		return errors.New("db user is required")
	}
	if c.Password == "" {
		return errors.New("db password is required")
	}
	if c.Database == "" {
		return errors.New("db database is required")
	}
	if c.Type != "" {
		switch strings.ToLower(c.Type) {
		case "oracle", "sqlserver", "mssql", "mysql":
			// ok
		default:
			return fmt.Errorf("db type %q is not supported (want mysql/oracle/sqlserver)", c.Type)
		}
	}
	return nil
}

// EffectivePort returns Port when set, otherwise the driver default.
// driver is the resolved driver name (e.g. "mysql", "oracle",
// "sqlserver"). Pass an empty string to fall back to Driver().
func (c *DBConfig) EffectivePort(driver string) int {
	if c.Port > 0 {
		return c.Port
	}
	if driver == "" {
		driver = c.Driver()
	}
	return c.DefaultPort(driver)
}

// rawHelper mirrors HelperConfig with the duration fields kept as raw JSON so
// we can apply flexible parsing rules.
type rawHelper struct {
	ServerURL          string          `json:"server_url"`
	Token              string          `json:"token"`
	ClientID           string          `json:"client_id,omitempty"`
	TomcatHome         string          `json:"tomcat_home"`
	JDKHome            string          `json:"jdk_home"`
	LogDir             string          `json:"log_dir"`
	AllowedDirs        []string        `json:"allowed_dirs"`
	CommandTimeout     json.RawMessage `json:"command_timeout"`
	MaxOutputSize      json.RawMessage `json:"max_output_size"`
	ReconnectInterval  json.RawMessage `json:"reconnect_interval"`
	HeartbeatInterval  json.RawMessage `json:"heartbeat_interval"`
	HeartbeatTimeout   json.RawMessage `json:"heartbeat_timeout"`
	LogFile            string          `json:"log_file"`
	InsecureSkipVerify bool            `json:"insecure_skip_verify"`
	ServiceName        string          `json:"service_name,omitempty"`
	ESB                *DBConfig       `json:"esb,omitempty"`
	ERP                *DBConfig       `json:"erp,omitempty"`
}

// parseDurationValue accepts either a JSON number (interpreted as seconds)
// or a string in time.ParseDuration syntax ("30s", "1m").
func parseDurationValue(raw json.RawMessage, def time.Duration) (time.Duration, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return def, nil
	}
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return 0, err
		}
		return time.ParseDuration(s)
	}
	var n int64
	if err := json.Unmarshal(raw, &n); err != nil {
		return 0, err
	}
	return time.Duration(n) * time.Second, nil
}

func parseBytesValue(raw json.RawMessage, def int64) (int64, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return def, nil
	}
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return 0, err
		}
		return humanBytes(s)
	}
	var n int64
	if err := json.Unmarshal(raw, &n); err != nil {
		return 0, err
	}
	return n, nil
}

func humanBytes(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, errors.New("empty byte size")
	}
	mult := int64(1)
	last := s[len(s)-1]
	switch last {
	case 'k', 'K':
		mult = 1 << 10
		s = s[:len(s)-1]
	case 'm', 'M':
		mult = 1 << 20
		s = s[:len(s)-1]
	case 'g', 'G':
		mult = 1 << 30
		s = s[:len(s)-1]
	}
	var n int64
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil {
		return 0, err
	}
	return n * mult, nil
}

// ServerConfig describes the local WebSocket relay.
type ServerConfig struct {
	Listen      string   `json:"listen"`
	WSListen    string   `json:"ws_listen"`
	TLSCert     string   `json:"tls_cert"`
	TLSKey      string   `json:"tls_key"`
	Tokens      []string `json:"tokens"`
	AuditLog    string   `json:"audit_log"`
	AgentListen string   `json:"agent_listen"`
	// ProblemLog is the markdown file that the server-local `report_problem`
	// MCP tool appends to. Relative paths resolve against server.exe's
	// working directory. Defaults to "problem.md".
	ProblemLog string `json:"problem_log"`
}

// LoadHelper reads config.json from path and validates it.
func LoadHelper(path string) (*HelperConfig, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read helper config: %w", err)
	}
	cfg, err := parseHelper(raw)
	if err != nil {
		return nil, fmt.Errorf("parse helper config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	cfg.LoadedAt = time.Now()
	return cfg, nil
}

// parseHelper parses helper config with friendlier rules for durations and
// sizes: durations accept either a JSON number of seconds or a Go duration
// string ("30s"); sizes accept plain numbers or a unit suffix ("1M", "512K").
func parseHelper(raw []byte) (*HelperConfig, error) {
	rh := &rawHelper{
		CommandTimeout:    json.RawMessage("30"),
		MaxOutputSize:     json.RawMessage("1048576"),
		ReconnectInterval: json.RawMessage("5"),
		HeartbeatInterval: json.RawMessage("30"),
		HeartbeatTimeout:  json.RawMessage("90"),
		LogFile:           "helper.log",
	}
	if err := json.Unmarshal(raw, rh); err != nil {
		return nil, err
	}
	cfg := &HelperConfig{
		ServerURL:          rh.ServerURL,
		Token:              rh.Token,
		ClientID:           rh.ClientID,
		TomcatHome:         rh.TomcatHome,
		JDKHome:            rh.JDKHome,
		LogDir:             rh.LogDir,
		AllowedDirs:        rh.AllowedDirs,
		LogFile:            rh.LogFile,
		InsecureSkipVerify: rh.InsecureSkipVerify,
		ServiceName:        rh.ServiceName,
		ESB:                rh.ESB,
		ERP:                rh.ERP,
	}
	var err error
	if cfg.CommandTimeout, err = parseDurationValue(rh.CommandTimeout, 30*time.Second); err != nil {
		return nil, fmt.Errorf("command_timeout: %w", err)
	}
	if cfg.MaxOutputSize, err = parseBytesValue(rh.MaxOutputSize, 1<<20); err != nil {
		return nil, fmt.Errorf("max_output_size: %w", err)
	}
	if cfg.ReconnectInterval, err = parseDurationValue(rh.ReconnectInterval, 5*time.Second); err != nil {
		return nil, fmt.Errorf("reconnect_interval: %w", err)
	}
	if cfg.HeartbeatInterval, err = parseDurationValue(rh.HeartbeatInterval, 30*time.Second); err != nil {
		return nil, fmt.Errorf("heartbeat_interval: %w", err)
	}
	if cfg.HeartbeatTimeout, err = parseDurationValue(rh.HeartbeatTimeout, 90*time.Second); err != nil {
		return nil, fmt.Errorf("heartbeat_timeout: %w", err)
	}
	return cfg, nil
}

// Validate enforces required fields and normalizes paths.
func (c *HelperConfig) Validate() error {
	if c.ServerURL == "" {
		return errors.New("server_url is required")
	}
	if !strings.HasPrefix(c.ServerURL, "wss://") && !strings.HasPrefix(c.ServerURL, "ws://") {
		return errors.New("server_url must start with ws:// or wss://")
	}
	if c.Token == "" {
		return errors.New("token is required")
	}
	if len(c.Token) < 8 {
		return errors.New("token is too short (need >= 8 chars)")
	}
	if c.TomcatHome == "" {
		return errors.New("tomcat_home is required")
	}
	if c.JDKHome == "" {
		return errors.New("jdk_home is required")
	}
	if c.LogDir == "" {
		return errors.New("log_dir is required")
	}
	if len(c.AllowedDirs) == 0 {
		return errors.New("allowed_dirs must list at least one directory")
	}
	if c.CommandTimeout <= 0 {
		c.CommandTimeout = 30 * time.Second
	}
	if c.MaxOutputSize <= 0 {
		c.MaxOutputSize = 1 << 20
	}
	if c.ReconnectInterval <= 0 {
		c.ReconnectInterval = 5 * time.Second
	}
	if c.HeartbeatInterval <= 0 {
		c.HeartbeatInterval = 30 * time.Second
	}
	if c.HeartbeatTimeout <= 0 {
		c.HeartbeatTimeout = 90 * time.Second
	}
	if c.HeartbeatTimeout <= c.HeartbeatInterval {
		return errors.New("heartbeat_timeout must be greater than heartbeat_interval")
	}
	if c.LogFile == "" {
		c.LogFile = "helper.log"
	}
	for i, d := range c.AllowedDirs {
		abs, err := filepath.Abs(d)
		if err != nil {
			return fmt.Errorf("allowed_dirs[%d] invalid: %w", i, err)
		}
		c.AllowedDirs[i] = abs
	}
	if abs, err := filepath.Abs(c.TomcatHome); err == nil {
		c.TomcatHome = abs
	}
	if abs, err := filepath.Abs(c.JDKHome); err == nil {
		c.JDKHome = abs
	}
	if abs, err := filepath.Abs(c.LogDir); err == nil {
		c.LogDir = abs
	}
	// DB connectors are optional. Validate only when the operator actually
	// configured one — bad config will be logged at startup, never fatal.
	if c.ESB != nil {
		if err := c.ESB.Validate(); err != nil {
			return fmt.Errorf("esb: %w", err)
		}
	}
	if c.ERP != nil {
		if err := c.ERP.Validate(); err != nil {
			return fmt.Errorf("erp: %w", err)
		}
	}
	return nil
}

// IsAllowedWorkDir returns true if workDir is inside one of AllowedDirs.
func (c *HelperConfig) IsAllowedWorkDir(workDir string) bool {
	if workDir == "" {
		return false
	}
	abs, err := filepath.Abs(workDir)
	if err != nil {
		return false
	}
	abs = filepath.Clean(abs)
	for _, d := range c.AllowedDirs {
		if rel, err := filepath.Rel(d, abs); err == nil && !strings.HasPrefix(rel, "..") && rel != ".." {
			return true
		}
	}
	return false
}

// LoadServer reads server config and applies defaults.
func LoadServer(path string) (*ServerConfig, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read server config: %w", err)
	}
	cfg := &ServerConfig{
		Listen:      ":443",
		AgentListen: "127.0.0.1:7777",
		AuditLog:    "audit.log",
		ProblemLog:  "problem.md",
	}
	if err := json.Unmarshal(raw, cfg); err != nil {
		return nil, fmt.Errorf("parse server config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Validate enforces required server fields.
func (s *ServerConfig) Validate() error {
	if s.Listen == "" {
		return errors.New("listen is required")
	}
	if s.TLSCert == "" || s.TLSKey == "" {
		return errors.New("tls_cert and tls_key are required")
	}
	if len(s.Tokens) == 0 {
		return errors.New("tokens must list at least one accepted token")
	}
	for i, t := range s.Tokens {
		if len(t) < 8 {
			return fmt.Errorf("tokens[%d] too short", i)
		}
	}
	if s.ProblemLog == "" {
		s.ProblemLog = "problem.md"
	}
	if abs, err := filepath.Abs(s.ProblemLog); err == nil {
		s.ProblemLog = abs
	}
	return nil
}

// HasToken returns true if token is accepted by the server.
func (s *ServerConfig) HasToken(token string) bool {
	for _, t := range s.Tokens {
		if t == token {
			return true
		}
	}
	return false
}
