package openai

import "time"

// ClientConfig configures an OpenAI-backed ai.Provider.
type ClientConfig struct {
	// Endpoint is the API base URL. Tests override this; production leaves it
	// empty and the client defaults to https://api.openai.com.
	Endpoint      string
	APIKey        string
	Model         string
	Timeout       time.Duration
	OCRPrompt     string
	SummaryPrompt string
}
