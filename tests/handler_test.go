package tests

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/seefs001/xox/xlog"
)

func TestHandleChat(t *testing.T) {
	client := &http.Client{}

	requestBody := map[string]interface{}{
		"model":  "meta/llama3.1-8b",
		"stream": true,
		"messages": []map[string]string{
			{
				"role":    "user",
				"content": "Hello, how are you?",
			},
		},
	}

	jsonBody, err := json.Marshal(requestBody)
	if err != nil {
		t.Fatalf("Failed to marshal request body: %v", err)
	}

	req, err := http.NewRequest("POST", "http://localhost:8080/v1/chat/completions", bytes.NewBuffer(jsonBody))
	if err != nil {
		t.Fatalf("Failed to create request: %v", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Failed to send request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected status OK; got %v", resp.Status)
	}

	reader := bufio.NewReader(resp.Body)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				break
			}
			t.Fatalf("Failed to read response: %v", err)
		}

		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		if strings.HasPrefix(line, "data: ") {
			data := strings.TrimPrefix(line, "data: ")
			if data == "[DONE]" {
				xlog.Info("Stream completed")
				break
			}

			var streamResponse map[string]interface{}
			err := json.Unmarshal([]byte(data), &streamResponse)
			if err != nil {
				xlog.Errorf("Failed to unmarshal stream response: %v", err)
				t.Fatalf("Failed to unmarshal stream response: %v", err)
			}

			xlog.Infof("Received stream response: %+v", streamResponse)

			if _, ok := streamResponse["choices"]; !ok {
				xlog.Error("Stream response does not contain 'choices' field")
				t.Error("Stream response does not contain 'choices' field")
			}
		}
	}
}
