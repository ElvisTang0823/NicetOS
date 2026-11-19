package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestBundledOllamaInferenceCompatibilityCheck(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		response   string
		wantErr    bool
	}{
		{
			name:       "successful inference",
			statusCode: http.StatusOK,
			response:   `{"message":{"content":"OK"}}`,
		},
		{
			name:       "empty inference response",
			statusCode: http.StatusOK,
			response:   `{"message":{"content":""}}`,
			wantErr:    true,
		},
		{
			name:       "inference API failure",
			statusCode: http.StatusInternalServerError,
			response:   `{}`,
			wantErr:    true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.Method != http.MethodPost || request.URL.Path != "/api/chat" {
					t.Errorf("unexpected inference request: %s %s", request.Method, request.URL.Path)
				}
				var payload struct {
					Model  string `json:"model"`
					Stream bool   `json:"stream"`
				}
				if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
					t.Errorf("decode inference request: %v", err)
				}
				if payload.Model != bundledModelName {
					t.Errorf("expected model %q, got %q", bundledModelName, payload.Model)
				}
				if payload.Stream {
					t.Error("expected inference compatibility request to disable streaming")
				}
				writer.WriteHeader(test.statusCode)
				_, _ = writer.Write([]byte(test.response))
			}))
			defer server.Close()

			runtime := &bundledOllama{apiURL: server.URL}
			err := runtime.checkModelInference()
			if (err != nil) != test.wantErr {
				t.Fatalf("checkModelInference() error = %v, wantErr %v", err, test.wantErr)
			}
		})
	}
}
