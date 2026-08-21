package agentbridge

type Event struct {
	Type               string           `json:"type"`
	SessionID          string           `json:"session_id,omitempty"`
	Text               string           `json:"text,omitempty"`
	Media              []MediaContent   `json:"media,omitempty"`
	Tool               *ToolEvent       `json:"tool,omitempty"`
	Permission         *PermissionEvent `json:"permission,omitempty"`
	Plan               *PlanEvent       `json:"plan,omitempty"`
	Retry              *RetryEvent      `json:"retry,omitempty"`
	Status             string           `json:"status,omitempty"`
	Model              string           `json:"model,omitempty"`
	StopReason         string           `json:"stop_reason,omitempty"`
	Error              string           `json:"error,omitempty"`
	SessionAutoApprove *bool            `json:"session_auto_approve,omitempty"`
	NeedsBootstrap     *bool            `json:"needs_bootstrap,omitempty"`
	UserTurnCount      *int             `json:"user_turn_count,omitempty"`
}

// MediaContent is a structured image/video payload forwarded from ACP to the
// browser. Data is base64 without a data: prefix; URI is used for remote or
// file-backed resources.
type MediaContent struct {
	Kind     string `json:"kind"` // image | video | audio | resource
	MimeType string `json:"mime_type,omitempty"`
	Data     string `json:"data,omitempty"`
	URI      string `json:"uri,omitempty"`
	Name     string `json:"name,omitempty"`
	Title    string `json:"title,omitempty"`
}

type RetryEvent struct {
	State         string `json:"state"`
	Attempt       int    `json:"attempt,omitempty"`
	MaxRetries    int    `json:"max_retries,omitempty"`
	Attempts      int    `json:"attempts,omitempty"`
	Reason        string `json:"reason,omitempty"`
	ErrorType     string `json:"error_type,omitempty"`
	Message       string `json:"message,omitempty"`
	IsRateLimited bool   `json:"is_rate_limited,omitempty"`
}

type ToolEvent struct {
	ID        string         `json:"id,omitempty"`
	Title     string         `json:"title,omitempty"`
	Kind      string         `json:"kind,omitempty"`
	Status    string         `json:"status,omitempty"`
	RawInput  any            `json:"raw_input,omitempty"`
	RawOutput any            `json:"raw_output,omitempty"`
	Media     []MediaContent `json:"media,omitempty"`
}

type PermissionEvent struct {
	RequestID string             `json:"request_id"`
	Summary   string             `json:"summary"`
	Tool      ToolEvent          `json:"tool"`
	Options   []PermissionOption `json:"options"`
}

type PermissionOption struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Kind string `json:"kind"`
}

// PlanEvent carries live plan entries and/or an exit_plan_mode approval gate.
type PlanEvent struct {
	RequestID string      `json:"request_id,omitempty"`
	Body      string      `json:"body,omitempty"`
	Entries   []PlanEntry `json:"entries,omitempty"`
	Waiting   bool        `json:"waiting,omitempty"`
}

type PlanEntry struct {
	Content  string `json:"content,omitempty"`
	Priority string `json:"priority,omitempty"`
	Status   string `json:"status,omitempty"`
}

// Attachment is a user-supplied file sent alongside a prompt.
// Prefer Path (server-side file from POST /api/agent/upload) for large payloads.
// Images may still use inline base64 Data for tiny pastes; text_file may use Text.
type Attachment struct {
	Kind     string `json:"kind"`           // "image" | "text_file" | "path"
	Data     string `json:"data,omitempty"` // base64 (no data: prefix) for images
	MimeType string `json:"mime_type,omitempty"`
	Name     string `json:"name,omitempty"`
	Text     string `json:"text,omitempty"` // file contents for text_file
	Path     string `json:"path,omitempty"` // absolute path on the switch host
}

type Status struct {
	Available          bool   `json:"available"`
	GrokPath           string `json:"grok_path,omitempty"`
	Running            bool   `json:"running"`
	State              string `json:"state"`
	SessionID          string `json:"session_id,omitempty"`
	Cwd                string `json:"cwd,omitempty"`
	DefaultCwd         string `json:"default_cwd,omitempty"`
	Busy               bool   `json:"busy"`
	AlwaysApprove      bool   `json:"always_approve"`
	SessionAutoApprove bool   `json:"session_auto_approve"`
	Model              string `json:"model,omitempty"`
	Error              string `json:"error,omitempty"`
	NeedsBootstrap     bool   `json:"needs_bootstrap,omitempty"`
	UserTurnCount      int    `json:"user_turn_count,omitempty"`
}

type StartOptions struct {
	Cwd           string `json:"cwd"`
	AlwaysApprove bool   `json:"always_approve"`
	SessionID     string `json:"session_id,omitempty"`
}

type RewindResult struct {
	OK            bool   `json:"ok"`
	Soft          bool   `json:"soft,omitempty"`
	Error         string `json:"error,omitempty"`
	UserTurnCount int    `json:"user_turn_count"`
	TargetIndex   int    `json:"target_index,omitempty"`
}

type PlanDecision struct {
	Outcome  string `json:"outcome"` // approved | cancelled | abandoned
	Feedback string `json:"feedback,omitempty"`
}
