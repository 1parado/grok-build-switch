package agentbridge

type Event struct {
	Type       string           `json:"type"`
	SessionID  string           `json:"session_id,omitempty"`
	Text       string           `json:"text,omitempty"`
	Tool       *ToolEvent       `json:"tool,omitempty"`
	Permission *PermissionEvent `json:"permission,omitempty"`
	Retry      *RetryEvent      `json:"retry,omitempty"`
	Status     string           `json:"status,omitempty"`
	Model      string           `json:"model,omitempty"`
	StopReason string           `json:"stop_reason,omitempty"`
	Error      string           `json:"error,omitempty"`
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
	ID        string `json:"id,omitempty"`
	Title     string `json:"title,omitempty"`
	Kind      string `json:"kind,omitempty"`
	Status    string `json:"status,omitempty"`
	RawInput  any    `json:"raw_input,omitempty"`
	RawOutput any    `json:"raw_output,omitempty"`
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

type Status struct {
	Available     bool   `json:"available"`
	GrokPath      string `json:"grok_path,omitempty"`
	Running       bool   `json:"running"`
	State         string `json:"state"`
	SessionID     string `json:"session_id,omitempty"`
	Cwd           string `json:"cwd,omitempty"`
	DefaultCwd    string `json:"default_cwd,omitempty"`
	Busy          bool   `json:"busy"`
	AlwaysApprove bool   `json:"always_approve"`
	Model         string `json:"model,omitempty"`
	Error         string `json:"error,omitempty"`
}

type StartOptions struct {
	Cwd           string `json:"cwd"`
	AlwaysApprove bool   `json:"always_approve"`
	SessionID     string `json:"session_id,omitempty"`
}
