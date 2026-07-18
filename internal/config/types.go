package config

import "time"

type Config struct {
	ListenAddr string `toml:"listen_addr" json:"listen_addr"`
	WebDAVUser string `toml:"webdav_user" json:"webdav_user"`
	WebDAVPass string `toml:"webdav_pass" json:"webdav_pass"`

	TemplateDir    string `toml:"template_dir" json:"template_dir"`
	VaultDir       string `toml:"vault_dir" json:"vault_dir"`
	DefaultPDFDir  string `toml:"default_pdf_dir" json:"default_pdf_dir"`
	DefaultNoteDir string `toml:"default_note_dir" json:"default_note_dir"`
	StateFile      string `toml:"state_file" json:"state_file"`

	AI     AIConfig     `toml:"ai" json:"ai"`
	Gemini GeminiConfig `toml:"gemini" json:"gemini"`
	OpenAI OpenAIConfig `toml:"openai" json:"openai"`
	Routes []Route      `toml:"route" json:"route"`
}

type Route struct {
	From     string `toml:"from" json:"from"`
	PDFDir   string `toml:"pdf_dir" json:"pdf_dir"`
	NoteDir  string `toml:"note_dir" json:"note_dir"`
	NoteName string `toml:"note_name" json:"note_name"`
	PDFName  string `toml:"pdf_name" json:"pdf_name"`
	Template string `toml:"template" json:"template"`
	AI       bool   `toml:"ai" json:"ai"`
	// TagMergeStrategy is "merge" by default; "replace" retains the legacy
	// filename-authoritative tag behavior.
	TagMergeStrategy string `toml:"tag_merge_strategy" json:"tag_merge_strategy"`
	// Nil means the default true. A pointer distinguishes an omitted value from
	// an explicit TOML false.
	PreserveMarkerOnAIFailure *bool `toml:"preserve_marker_on_ai_failure" json:"preserve_marker_on_ai_failure"`
}

type GeminiConfig struct {
	APIKeyFile    string `toml:"api_key_file" json:"api_key_file"`
	Model         string `toml:"model" json:"model"`
	Timeout       string `toml:"timeout" json:"timeout"`
	OCRPrompt     string `toml:"ocr_prompt" json:"ocr_prompt"`
	SummaryPrompt string `toml:"summary_prompt" json:"summary_prompt"`
	// MinReprocessInterval, when set, suppresses AI re-processing for a
	// route/output-path match whose full SHA256 changed but whose last AI
	// run succeeded within this interval. Empty/"0s" disables debouncing
	// (default: disabled, preserving current behavior exactly).
	MinReprocessInterval         string        `toml:"min_reprocess_interval" json:"min_reprocess_interval"`
	MinReprocessIntervalDuration time.Duration `toml:"-" json:"-"`

	Retry RetryConfig `toml:"retry" json:"retry"`
}

// RetryConfig holds configuration for automatic AI retry on failure.
// BackoffDuration is populated by validate() from Backoff and is not a TOML field.
type RetryConfig struct {
	Enabled         bool          `toml:"enabled" json:"enabled"`
	MaxRetries      int           `toml:"max_retries" json:"max_retries"`
	Backoff         string        `toml:"backoff" json:"backoff"`
	BackoffDuration time.Duration `toml:"-" json:"-"`
}

type AIConfig struct {
	Provider string `toml:"provider" json:"provider"`
}

type OpenAIConfig struct {
	APIKeyFile    string `toml:"api_key_file" json:"api_key_file"`
	Model         string `toml:"model" json:"model"`
	Timeout       string `toml:"timeout" json:"timeout"`
	OCRPrompt     string `toml:"ocr_prompt" json:"ocr_prompt"`
	SummaryPrompt string `toml:"summary_prompt" json:"summary_prompt"`
}
