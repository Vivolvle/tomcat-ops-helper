// Package protocol defines the wire format exchanged between helper and server.
package protocol

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"
)

// MessageType enumerates the kinds of envelopes that travel over the wire.
type MessageType string

const (
	TypeHello    MessageType = "hello"
	TypeToolList MessageType = "toollist"
	TypeCommand  MessageType = "command"
	TypeResult   MessageType = "result"
	TypePing     MessageType = "ping"
	TypePong     MessageType = "pong"
)

// Envelope is the JSON wire container. Exactly one of Hello/ToolList/Command/Result
// carries a payload; everything else is ignored when the type does not match.
type Envelope struct {
	Type      MessageType     `json:"type"`
	ID        string          `json:"id,omitempty"`
	Action    string          `json:"action,omitempty"`
	Params    json.RawMessage `json:"params,omitempty"`
	Success   *bool           `json:"success,omitempty"`
	Data      json.RawMessage `json:"data,omitempty"`
	Error     string          `json:"error,omitempty"`
	TookMs    int64           `json:"took_ms,omitempty"`
	Timestamp int64           `json:"ts,omitempty"`

	// Hello-only fields.
	ClientID  string `json:"client_id,omitempty"`
	Token     string `json:"token,omitempty"`
	Version   string `json:"version,omitempty"`
	Hostname  string `json:"hostname,omitempty"`

	// ToolList-only field. Marshalled as `tools` on the wire.
	Tools []ToolMeta `json:"tools,omitempty"`
}

// Hello is the very first message sent by the helper on connect.
type Hello struct {
	ClientID string `json:"client_id"`
	Token    string `json:"token"`
	Version  string `json:"version"`
	Hostname string `json:"hostname"`
	SentAt   int64  `json:"sent_at"`
}

// ToolMeta mirrors actions.Tool but lives in protocol so the wire format does
// not depend on internal package layout.
type ToolMeta struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// ToolList is sent by helper right after Hello, advertising every action it
// supports. The server caches it indexed by client_id and exposes it through
// its MCP-style /tools endpoint.
type ToolList struct {
	Tools []ToolMeta `json:"tools"`
}

// Command is a request from server to helper.
type Command struct {
	ID     string          `json:"id"`
	Action string          `json:"action"`
	Params json.RawMessage `json:"params,omitempty"`
}

// Result is the response for a Command. Exactly one of Data/Error is meaningful
// at a time; Success mirrors the bool return of the action.
type Result struct {
	ID      string          `json:"id"`
	Success bool            `json:"success"`
	Data    json.RawMessage `json:"data,omitempty"`
	Error   string          `json:"error,omitempty"`
	TookMs  int64           `json:"took_ms,omitempty"`
}

// NewID returns a 16-byte hex identifier suitable for command/result correlation.
func NewID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return time.Now().UTC().Format("20060102T150405.000000000")
	}
	return hex.EncodeToString(b[:])
}

// DecodeEnvelope parses raw bytes into an Envelope and validates the type tag.
func DecodeEnvelope(raw []byte) (*Envelope, error) {
	if len(raw) == 0 {
		return nil, errors.New("protocol: empty payload")
	}
	var env Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, err
	}
	if env.Type == "" {
		return nil, errors.New("protocol: missing type")
	}
	return &env, nil
}

// Encode produces wire bytes for any value. Used by both helper and server.
func Encode(v any) ([]byte, error) {
	return json.Marshal(v)
}

// ToEnvelope converts a typed message to the wire Envelope.
func (h Hello) ToEnvelope() *Envelope {
	return &Envelope{
		Type:      TypeHello,
		ClientID:  h.ClientID,
		Token:     h.Token,
		Version:   h.Version,
		Hostname:  h.Hostname,
		Timestamp: h.SentAt,
	}
}

// ToEnvelope converts a ToolList to the wire Envelope.
func (t ToolList) ToEnvelope() *Envelope {
	return &Envelope{
		Type:  TypeToolList,
		Tools: t.Tools,
	}
}

// ToolListFromEnvelope extracts a ToolList payload from an envelope.
func ToolListFromEnvelope(env *Envelope) (ToolList, error) {
	if env.Type != TypeToolList {
		return ToolList{}, errors.New("protocol: not a toollist envelope")
	}
	return ToolList{Tools: env.Tools}, nil
}

// ToEnvelope converts a Command into the wire Envelope.
func (c Command) ToEnvelope() *Envelope {
	return &Envelope{
		Type:   TypeCommand,
		ID:     c.ID,
		Action: c.Action,
		Params: c.Params,
	}
}

// ToEnvelope converts a Result into the wire Envelope.
func (r Result) ToEnvelope() *Envelope {
	env := &Envelope{
		Type:    TypeResult,
		ID:      r.ID,
		Error:   r.Error,
		TookMs:  r.TookMs,
	}
	env.Success = &r.Success
	if len(r.Data) > 0 {
		env.Data = r.Data
	}
	return env
}

// FromEnvelope extracts a Hello payload from an envelope.
func HelloFromEnvelope(env *Envelope) (Hello, error) {
	if env.Type != TypeHello {
		return Hello{}, errors.New("protocol: not a hello envelope")
	}
	return Hello{
		ClientID: env.ClientID,
		Token:    env.Token,
		Version:  env.Version,
		Hostname: env.Hostname,
		SentAt:   env.Timestamp,
	}, nil
}

// FromEnvelope extracts a Command payload from an envelope.
func CommandFromEnvelope(env *Envelope) (Command, error) {
	if env.Type != TypeCommand {
		return Command{}, errors.New("protocol: not a command envelope")
	}
	if env.ID == "" {
		return Command{}, errors.New("protocol: command missing id")
	}
	if env.Action == "" {
		return Command{}, errors.New("protocol: command missing action")
	}
	return Command{
		ID:     env.ID,
		Action: env.Action,
		Params: env.Params,
	}, nil
}

// FromEnvelope extracts a Result payload from an envelope.
func ResultFromEnvelope(env *Envelope) (Result, error) {
	if env.Type != TypeResult {
		return Result{}, errors.New("protocol: not a result envelope")
	}
	if env.ID == "" {
		return Result{}, errors.New("protocol: result missing id")
	}
	r := Result{ID: env.ID, Error: env.Error, TookMs: env.TookMs}
	if env.Success != nil {
		r.Success = *env.Success
	}
	r.Data = env.Data
	return r, nil
}
