package grok

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

type ChatRequest struct {
	Prompt string
	ModeID string
}

type ChatResult struct {
	Text         string
	FirstTokenMS *int
}

func (c *Client) Chat(ctx context.Context, req ChatRequest, onToken func(string) error) (*ChatResult, error) {
	prompt := strings.TrimSpace(req.Prompt)
	if prompt == "" {
		return nil, errors.New("prompt is required")
	}
	payload := buildChatPayload(prompt, req.ModeID)
	start := time.Now()
	resp, err := c.postStream(ctx, ChatURL, BaseURL+"/", payload)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	var chunks []string
	var firstTokenMS *int
	err = readStreamLines(ctx, resp.Body, func(data string) error {
		if msg := extractStreamError(data); msg != "" {
			return &UpstreamError{StatusCode: 502, Message: msg}
		}
		token := extractTextFragment(data)
		if token == "" {
			return nil
		}
		if firstTokenMS == nil {
			ms := int(time.Since(start).Milliseconds())
			firstTokenMS = &ms
		}
		chunks = append(chunks, token)
		if onToken != nil {
			return onToken(token)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &ChatResult{
		Text:         strings.Join(chunks, ""),
		FirstTokenMS: firstTokenMS,
	}, nil
}

func extractTextFragment(data string) string {
	var obj struct {
		Result struct {
			Response struct {
				Token      any    `json:"token"`
				IsThinking bool   `json:"isThinking"`
				MessageTag string `json:"messageTag"`
			} `json:"response"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(data), &obj); err != nil {
		return ""
	}
	if obj.Result.Response.Token == nil || obj.Result.Response.IsThinking || obj.Result.Response.MessageTag != "final" {
		return ""
	}
	switch v := obj.Result.Response.Token.(type) {
	case string:
		return v
	default:
		return strings.TrimSpace(strings.Trim(strings.ReplaceAll(strings.TrimSpace(string(mustJSON(v))), "\\n", "\n"), "\""))
	}
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}
