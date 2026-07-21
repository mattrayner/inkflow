package openai

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"inkflow/internal/ai"
)

const defaultEndpoint = "https://api.openai.com"

// Compile-time interface compliance check.
var _ ai.Provider = (*Client)(nil)

// Client implements ai.Provider against the OpenAI Responses API.
type Client struct {
	cfg        ClientConfig
	httpClient *http.Client
}

// New builds a Client. The returned value is safe for concurrent use.
func New(cfg ClientConfig) *Client {
	return &Client{
		cfg:        cfg,
		httpClient: &http.Client{Timeout: cfg.Timeout},
	}
}

// Process sends pdf to OpenAI and returns the parsed result.
func (c *Client) Process(ctx context.Context, pdf io.Reader) (ai.Result, error) {
	pdfData, err := io.ReadAll(pdf)
	if err != nil {
		return ai.Result{}, fmt.Errorf("read pdf: %w", err)
	}

	body, err := json.Marshal(c.buildRequest(pdfData))
	if err != nil {
		return ai.Result{}, fmt.Errorf("encode request: %w", err)
	}

	endpoint := c.cfg.Endpoint
	if endpoint == "" {
		endpoint = defaultEndpoint
	}
	url := strings.TrimRight(endpoint, "/") + "/v1/responses"

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return ai.Result{}, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	// Auth via header rather than a URL query parameter: keeps the API key out
	// of any URL that net/http includes in transport errors, which the importer
	// would otherwise write verbatim into the Obsidian note.
	req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return ai.Result{}, fmt.Errorf("openai request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return ai.Result{}, fmt.Errorf("read response body: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return ai.Result{}, fmt.Errorf("openai %w", &ai.APIError{
			StatusCode: resp.StatusCode,
			Message:    extractErrorMessage(respBody),
		})
	}

	return parseResponse(respBody)
}

func (c *Client) buildRequest(pdfData []byte) map[string]any {
	prompt := strings.TrimSpace(c.cfg.OCRPrompt) + "\n\n" +
		strings.TrimSpace(c.cfg.SummaryPrompt)

	return map[string]any{
		"model": c.cfg.Model,
		"input": []map[string]any{{
			"role": "user",
			"content": []map[string]any{
				{"type": "input_text", "text": prompt},
				{
					"type":      "input_file",
					"filename":  "upload.pdf",
					"file_data": "data:application/pdf;base64," + base64.StdEncoding.EncodeToString(pdfData),
				},
			},
		}},
		"text": map[string]any{
			"format": map[string]any{
				"type":   "json_schema",
				"name":   "ocr_result",
				"strict": true,
				"schema": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"ocr_text": map[string]any{"type": "string"},
						"summary": map[string]any{
							"type":  "array",
							"items": map[string]any{"type": "string"},
						},
					},
					"required":             []string{"ocr_text", "summary"},
					"additionalProperties": false,
				},
			},
		},
	}
}

type responsesResponse struct {
	Output []struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"output"`
}

type innerPayload struct {
	OCRText string   `json:"ocr_text"`
	Summary []string `json:"summary"`
}

func parseResponse(body []byte) (ai.Result, error) {
	var outer responsesResponse
	if err := json.Unmarshal(body, &outer); err != nil {
		return ai.Result{}, fmt.Errorf("parse outer response: %w", err)
	}
	for _, output := range outer.Output {
		for _, content := range output.Content {
			if content.Type != "output_text" {
				continue
			}
			var inner innerPayload
			if err := json.Unmarshal([]byte(content.Text), &inner); err != nil {
				return ai.Result{}, fmt.Errorf("parse JSON-mode payload: %w", err)
			}
			summary := make([]string, len(inner.Summary))
			for i, s := range inner.Summary {
				summary[i] = unescapeNewlines(s)
			}
			return ai.Result{OCR: unescapeNewlines(inner.OCRText), Summary: summary}, nil
		}
	}
	return ai.Result{}, fmt.Errorf("parse response: no output_text")
}

// unescapeNewlines repairs OpenAI's JSON-mode output when the model
// double-escapes newlines (writes `\\n` in its JSON, which parses to two
// literal characters `\` and `n` instead of a real newline). Handwritten
// notes essentially never contain a literal backslash-n, so converting
// every occurrence is a safe defensive fix.
func unescapeNewlines(s string) string {
	return strings.ReplaceAll(s, `\n`, "\n")
}

// extractErrorMessage pulls error.message out of an OpenAI error body so the
// importer doesn't paste 30 lines of JSON into the note. Falls back to the
// trimmed raw body if parsing fails or the field is missing.
func extractErrorMessage(body []byte) string {
	var parsed struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &parsed); err == nil && parsed.Error.Message != "" {
		return parsed.Error.Message
	}
	return strings.TrimSpace(string(body))
}
