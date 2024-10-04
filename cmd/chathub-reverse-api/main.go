package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/seefs001/xox/x"
	"github.com/seefs001/xox/xhttp"
	"github.com/seefs001/xox/xhttpc"
	"github.com/seefs001/xox/xlog"
)

// ModelMapping maps input model names to actual model names
var ModelMapping = map[string]string{
	"gpt-3.5-turbo":     "meta/llama3.1-8b",
	"gpt-3.5-turbo-16k": "meta/llama3.1-8b",
	"gpt-4o":            "meta/llama3.1-8b",
	"gpt-4-32k":         "meta/llama3.1-8b",
	"gpt-4o-mini":       "openai/gpt-4o-mini",
}

func main() {
	ctx := context.Background()
	if err := run(ctx); err != nil {
		xlog.Errorf("error running app: %v", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	mux := http.NewServeMux()

	mux.HandleFunc("/", xhttp.Wrap(func(ctx *xhttp.Context) {
		ctx.JSON(http.StatusOK, map[string]any{
			"message": "Hello world!",
		})
	}))

	mux.HandleFunc("/v1/chat/completions", xhttp.Wrap(handleChat))

	srv := &http.Server{
		Addr:    ":8080",
		Handler: mux,
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			xlog.Errorf("error listening and serving: %v", err)
		}
	}()

	xlog.Info("Server started on :8080")

	<-stop

	xlog.Info("Shutting down server...")

	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		xlog.Errorf("Server forced to shutdown: %v", err)
		return err
	}

	xlog.Info("Server exiting")
	return nil
}

type ChatCompletionRequest struct {
	Model    string    `json:"model"`
	Messages []Message `json:"messages"`
}

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type ChatCompletionChunk struct {
	ID                string   `json:"id"`
	Object            string   `json:"object"`
	Created           int64    `json:"created"`
	Model             string   `json:"model"`
	SystemFingerprint string   `json:"system_fingerprint"`
	Choices           []Choice `json:"choices"`
}

type Choice struct {
	Index        int     `json:"index"`
	Delta        Delta   `json:"delta"`
	LogProbs     *string `json:"logprobs"`
	FinishReason *string `json:"finish_reason"`
}

type Delta struct {
	Role    string `json:"role,omitempty"`
	Content string `json:"content,omitempty"`
}

func handleChat(ctx *xhttp.Context) {
	var req ChatCompletionRequest
	if err := ctx.Bind(&req); err != nil {
		xlog.Errorf("Invalid request body: %v", err)
		ctx.JSON(http.StatusBadRequest, map[string]string{"error": "Invalid request body"})
		return
	}

	xlog.Infof("Received request: %v", x.MustToJSON(req))

	resultChan, errChan := sendRequestToChatHub(ctx.Request.Context(), req)

	ctx.Writer.Header().Set("Content-Type", "text/event-stream")
	ctx.Writer.Header().Set("Cache-Control", "no-cache")
	ctx.Writer.Header().Set("Connection", "keep-alive")
	ctx.Writer.WriteHeader(http.StatusOK)

	flusher, ok := ctx.Writer.ResponseWriter.(http.Flusher)
	if !ok {
		xlog.Error("Streaming unsupported")
		ctx.JSON(http.StatusInternalServerError, map[string]string{"error": "Streaming unsupported"})
		return
	}

	for {
		select {
		case chunk, ok := <-resultChan:
			if !ok {
				fmt.Fprintf(ctx.Writer, "data: [DONE]\n\n")
				flusher.Flush()
				return
			}
			data, err := json.Marshal(chunk)
			if err != nil {
				xlog.Errorf("Failed to marshal chunk: %v", err)
				continue
			}
			fmt.Fprintf(ctx.Writer, "data: %s\n\n", data)
			flusher.Flush()
		case err, ok := <-errChan:
			if !ok {
				fmt.Fprintf(ctx.Writer, "data: [DONE]\n\n")
				flusher.Flush()
				return
			}
			xlog.Errorf("Error in request: %v", err)
			fmt.Fprintf(ctx.Writer, "data: {\"error\": \"%v\"}\n\n", err)
			flusher.Flush()
			return
		}
	}
}

func sendRequestToChatHub(ctx context.Context, req ChatCompletionRequest) (chan ChatCompletionChunk, chan error) {
	resultChan := make(chan ChatCompletionChunk, 1)
	errChan := make(chan error, 1)
	client, err := xhttpc.NewClient()
	if err != nil {
		xlog.Errorf("Failed to create client: %v", err)
		errChan <- fmt.Errorf("failed to create client: %v", err)
		return nil, nil
	}

	go func() {
		defer close(resultChan)
		defer close(errChan)

		actualModel, ok := ModelMapping[req.Model]
		if !ok {
			actualModel = req.Model // Use the original model if not found in mapping
		}
		req := map[string]interface{}{
			"model":    actualModel,
			"messages": req.Messages,
			"tools": []string{
				"image_generation",
			},
		}

		client.SetHeaders(map[string]string{
			"Content-Type":       "application/json",
			"Accept":             "*/*",
			"Accept-Language":    "zh-CN,zh;q=0.9",
			"Priority":           "u=1, i",
			"Sec-CH-UA":          "\"Chromium\";v=\"129\", \"Not=A?Brand\";v=\"8\"",
			"Sec-CH-UA-Mobile":   "?0",
			"Sec-CH-UA-Platform": "\"macOS\"",
			"Sec-Fetch-Dest":     "empty",
			"Sec-Fetch-Mode":     "cors",
			"Sec-Fetch-Site":     "same-origin",
			"X-App-ID":           "web",
			"X-Device-ID":        "db043b73-ee1b-49f0-a0f1-5f709d87c06d",
		})

		// Read and set cookie from file
		cookieContent, err := os.ReadFile("data/cookie.txt")
		if err != nil {
			xlog.Errorf("Failed to read cookie file: %v", err)
			errChan <- fmt.Errorf("failed to read cookie file: %v", err)
			return
		}
		client.SetHeader("Cookie", strings.TrimSpace(string(cookieContent)))
		resp, err := client.PostJSON(ctx, "https://app.chathub.gg/api/v3/chat/completions", req)

		if err != nil {
			xlog.Errorf("Failed to  request: %v", err)
			errChan <- fmt.Errorf("failed to request: %v", err)
			return
		}

		defer resp.Body.Close()

		contentType := resp.Header.Get("Content-Type")
		if contentType == "application/json" {
			var jsonResponse ChatCompletionChunk
			if err := json.NewDecoder(resp.Body).Decode(&jsonResponse); err != nil {
				xlog.Errorf("Failed to decode JSON response: %v", err)
				errChan <- fmt.Errorf("failed to decode JSON response: %v", err)
				return
			}
			resultChan <- jsonResponse
		} else {
			reader := bufio.NewReader(resp.Body)
			for {
				select {
				case <-ctx.Done():
					xlog.Info("Context canceled, stopping stream processing")
					return
				default:
					line, err := reader.ReadBytes('\n')
					if err != nil {
						if err == io.EOF {
							return
						}
						if errors.Is(err, context.Canceled) {
							xlog.Info("Context canceled while reading response")
							return
						}
						xlog.Errorf("Error reading response: %v", err)
						errChan <- fmt.Errorf("error reading response: %v", err)
						return
					}

					if bytes.HasPrefix(line, []byte("data: ")) {
						data := bytes.TrimPrefix(line, []byte("data: "))
						var message struct {
							Type      string `json:"type"`
							TextDelta string `json:"textDelta"`
						}
						if err := json.Unmarshal(data, &message); err != nil {
							xlog.Errorf("Error unmarshaling message: %v", err)
							errChan <- fmt.Errorf("error unmarshaling message: %v", err)
							continue
						}
						if message.Type == "text-delta" {
							chunk := ChatCompletionChunk{
								ID:      "chatcmpl-123",
								Object:  "chat.completion.chunk",
								Created: time.Now().Unix(),
								Model:   actualModel,
								Choices: []Choice{
									{
										Index: 0,
										Delta: Delta{
											Content: message.TextDelta,
										},
									},
								},
							}
							resultChan <- chunk
						} else if message.Type == "done" {
							return
						}
					}
				}
			}
		}
	}()

	return resultChan, errChan
}
