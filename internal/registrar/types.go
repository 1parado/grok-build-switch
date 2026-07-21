package registrar

import "time"

const configVersion = 1

type Config struct {
	Version                int    `json:"version"`
	BrowserPath            string `json:"browser_path"`
	BrowserMode            string `json:"browser_mode"`
	ProxyURL               string `json:"proxy_url"`
	EmailProvider          string `json:"email_provider"`
	DefaultDomains         string `json:"default_domains"`
	CloudmailURL           string `json:"cloudmail_url"`
	CloudmailAdminEmail    string `json:"cloudmail_admin_email"`
	CloudmailPassword      string `json:"cloudmail_password"`
	CloudflareAPIBase      string `json:"cloudflare_api_base"`
	CloudflareAPIKey       string `json:"cloudflare_api_key"`
	CloudflareAuthMode     string `json:"cloudflare_auth_mode"`
	CloudflareDomainsPath  string `json:"cloudflare_path_domains"`
	CloudflareAccountsPath string `json:"cloudflare_path_accounts"`
	CloudflareTokenPath    string `json:"cloudflare_path_token"`
	CloudflareMessagesPath string `json:"cloudflare_path_messages"`
	HotmailAccountsText    string `json:"hotmail_accounts_text"`
	HotmailMaxAliases      int    `json:"hotmail_max_aliases"`
	Count                  int    `json:"count"`
	Workers                int    `json:"workers"`
	MailTimeoutSeconds     int    `json:"mail_timeout_seconds"`
	PageTimeoutSeconds     int    `json:"page_timeout_seconds"`
	PreferProtocolMint     bool   `json:"prefer_protocol_mint"`
	ProtocolOnly           bool   `json:"protocol_only"`
	LastJobID              string `json:"last_job_id,omitempty"`
}

type JobStatus string

const (
	StatusStarting  JobStatus = "starting"
	StatusRunning   JobStatus = "running"
	StatusSucceeded JobStatus = "succeeded"
	StatusFailed    JobStatus = "failed"
	StatusCancelled JobStatus = "cancelled"
)

type AccountResult struct {
	Email      string `json:"email"`
	Status     string `json:"status"`
	MintMethod string `json:"mint_method,omitempty"`
	Error      string `json:"error,omitempty"`
	AuthFile   string `json:"auth_file,omitempty"`
}

type Job struct {
	ID         string          `json:"id"`
	Status     JobStatus       `json:"status"`
	Requested  int             `json:"requested"`
	Completed  int             `json:"completed"`
	Succeeded  int             `json:"succeeded"`
	Failed     int             `json:"failed"`
	Imported   int             `json:"imported,omitempty"`
	Updated    int             `json:"updated,omitempty"`
	Error      string          `json:"error,omitempty"`
	LogTail    []string        `json:"log_tail"`
	Results    []AccountResult `json:"results"`
	StartedAt  time.Time       `json:"started_at"`
	FinishedAt time.Time       `json:"finished_at,omitempty,omitzero"`
}

type State struct {
	Config       Config `json:"config"`
	Job          *Job   `json:"job,omitempty"`
	AuthDir      string `json:"auth_dir,omitempty"`
	AccountsPath string `json:"accounts_path"`
}

type ProbeCheck struct {
	Name     string `json:"name"`
	OK       bool   `json:"ok"`
	Required bool   `json:"required"`
	Detail   string `json:"detail"`
}

type ProbeResult struct {
	OK     bool         `json:"ok"`
	Checks []ProbeCheck `json:"checks"`
}

func DefaultConfig() Config {
	return Config{
		Version:                configVersion,
		BrowserMode:            "visible",
		EmailProvider:          "cloudflare",
		CloudflareAuthMode:     "none",
		CloudflareDomainsPath:  "/api/domains",
		CloudflareAccountsPath: "/api/new_address",
		CloudflareTokenPath:    "/api/token",
		CloudflareMessagesPath: "/api/mails",
		HotmailMaxAliases:      5,
		Count:                  1,
		Workers:                1,
		MailTimeoutSeconds:     180,
		PageTimeoutSeconds:     300,
		PreferProtocolMint:     true,
	}
}
