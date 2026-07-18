package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"inkflow/internal/ai"
)

func newTestClient(t *testing.T, handler http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	c := New(ClientConfig{
		Endpoint:      srv.URL,
		APIKey:        "test-key",
		Model:         "gpt-4.1",
		Timeout:       2 * time.Second,
		OCRPrompt:     "Transcribe faithfully",
		SummaryPrompt: "Summarize as 3 bullets",
	})
	return c, srv
}

func TestProcessHappyPathParsesJSONResponse(t *testing.T) {
	var gotPath, gotQuery, gotAuthorization, gotContentType string
	var gotBody []byte
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		gotAuthorization = r.Header.Get("Authorization")
		gotContentType = r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		inner, _ := json.Marshal(map[string]any{
			"ocr_text": "full transcription",
			"summary":  []string{"a", "b", "c"},
		})
		_ = json.NewEncoder(w).Encode(map[string]any{
			"output": []map[string]any{{
				"content": []map[string]any{{"type": "output_text", "text": string(inner)}},
			}},
		})
	})

	res, err := c.Process(context.Background(), bytes.NewReader([]byte("fake pdf bytes")))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.OCR != "full transcription" {
		t.Errorf("OCR = %q", res.OCR)
	}
	if len(res.Summary) != 3 || res.Summary[0] != "a" {
		t.Errorf("Summary = %v", res.Summary)
	}
	if gotPath != "/v1/responses" {
		t.Errorf("path = %q", gotPath)
	}
	if gotAuthorization != "Bearer test-key" {
		t.Errorf("Authorization = %q", gotAuthorization)
	}
	if strings.Contains(gotQuery, "test-key") {
		t.Errorf("API key leaked into query string: %q", gotQuery)
	}
	if gotContentType != "application/json" {
		t.Errorf("Content-Type = %q", gotContentType)
	}
	if !bytes.Contains(gotBody, []byte(`"type":"input_file"`)) {
		t.Errorf("input_file missing from request body: %s", gotBody)
	}
	if !bytes.Contains(gotBody, []byte(`"file_data":"data:application/pdf;base64,`)) {
		t.Errorf("base64 PDF data missing from request body: %s", gotBody)
	}
	if !bytes.Contains(gotBody, []byte(`"type":"json_schema"`)) ||
		!bytes.Contains(gotBody, []byte(`"additionalProperties":false`)) {
		t.Errorf("strict JSON schema missing from request body: %s", gotBody)
	}
	if !bytes.Contains(gotBody, []byte("Summarize as 3 bullets")) {
		t.Errorf("summary prompt missing: %s", gotBody)
	}
}

func TestProcessAuthFailure(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"API key invalid"}}`))
	})
	_, err := c.Process(context.Background(), bytes.NewReader([]byte("x")))
	if err == nil {
		t.Fatal("expected error")
	}
	if err.Error() != "openai 401: API key invalid" {
		t.Fatalf("expected clean error message, got: %v", err)
	}
	var apiErr *ai.APIError
	if !errors.As(err, &apiErr) {
		t.Fatal("expected shared ai.APIError")
	}
	if apiErr.StatusCode != http.StatusUnauthorized || apiErr.Message != "API key invalid" {
		t.Errorf("APIError = %#v, want status 401 and message %q", apiErr, "API key invalid")
	}
}

func TestProcessAPIErrorRetryability(t *testing.T) {
	tests := []struct {
		status int
		want   bool
	}{
		{http.StatusBadRequest, false},
		{http.StatusUnauthorized, false},
		{http.StatusForbidden, false},
		{http.StatusTooManyRequests, true},
		{http.StatusInternalServerError, true},
	}
	for _, tt := range tests {
		t.Run(http.StatusText(tt.status), func(t *testing.T) {
			c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(`{"error":{"message":"provider error"}}`))
			})
			_, err := c.Process(context.Background(), bytes.NewReader([]byte("x")))
			if err == nil {
				t.Fatal("expected error")
			}
			var apiErr *ai.APIError
			if !errors.As(err, &apiErr) {
				t.Fatal("expected shared ai.APIError")
			}
			if apiErr.StatusCode != tt.status {
				t.Errorf("status = %d, want %d", apiErr.StatusCode, tt.status)
			}
			if got := ai.IsRetryable(err); got != tt.want {
				t.Errorf("ai.IsRetryable(%v) = %v, want %v", err, got, tt.want)
			}
		})
	}
}

func TestProcessSchemaViolation(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"output": []map[string]any{{
				"content": []map[string]any{{"type": "output_text", "text": "not json"}},
			}},
		})
	})
	_, err := c.Process(context.Background(), bytes.NewReader([]byte("x")))
	if err == nil || !strings.Contains(err.Error(), "parse JSON-mode payload") {
		t.Fatalf("expected descriptive parse error, got %v", err)
	}
}

func TestProcessUnescapesDoublyEscapedNewlines(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		inner, _ := json.Marshal(map[string]any{
			"ocr_text": `line one\nline two`,
			"summary":  []string{`bullet\nwith escaped newline`},
		})
		_ = json.NewEncoder(w).Encode(map[string]any{
			"output": []map[string]any{{
				"content": []map[string]any{{"type": "output_text", "text": string(inner)}},
			}},
		})
	})
	res, err := c.Process(context.Background(), bytes.NewReader([]byte("x")))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.OCR, `\n`) || strings.Contains(res.Summary[0], `\n`) {
		t.Errorf("literal escaped newline was not repaired: %#v", res)
	}
}
