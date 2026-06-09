package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/gin-gonic/gin"
)

func (s *GeminiMessagesCompatService) forwardGeminiWebChatCompletions(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	req *apicompat.ChatCompletionsRequest,
	originalBody []byte,
	startTime time.Time,
) (*ForwardResult, error) {
	if req.Stream {
		return nil, s.writeChatCompletionsError(c, http.StatusBadRequest, "invalid_request_error", "Gemini Web Cookie mode currently supports non-streaming Chat Completions only")
	}
	if s.httpUpstream == nil {
		return nil, s.writeChatCompletionsError(c, http.StatusBadGateway, "upstream_error", "Gemini Web HTTP upstream is not configured")
	}

	prompt, err := geminiWebPromptFromChatCompletions(req)
	if err != nil {
		return nil, s.writeChatCompletionsError(c, http.StatusBadRequest, "invalid_request_error", err.Error())
	}

	isImage := isImageGenerationModel(req.Model)
	isVideo := geminiWebIsVideoRequest(req.Model)
	if isVideo && !geminiWebAccountCanGenerateVideo(account) {
		return nil, s.writeChatCompletionsError(c, http.StatusForbidden, "permission_error", geminiWebVideoTierError(account).Error())
	}

	client, err := newGeminiWebOfficialClient(account, s.httpUpstream)
	if err != nil {
		return nil, s.writeChatCompletionsError(c, http.StatusBadRequest, "invalid_request_error", err.Error())
	}

	var result *geminiWebGenerateResult
	if isImage {
		result, err = client.GenerateImage(ctx, prompt)
		if err == nil {
			if result == nil || strings.TrimSpace(result.ImageURL) == "" {
				err = errors.New("Gemini Web image did not return a downloadable image URL")
			} else if dataURL, _, dataErr := client.fetchGeminiImageDataURL(ctx, result.ImageURL); dataErr != nil || dataURL == "" {
				err = errors.New("Gemini Web image URL was generated but could not be downloaded by the backend")
			} else {
				result.ImageURL = dataURL
			}
		}
	} else if isVideo {
		result, err = client.GenerateVideo(ctx, prompt)
	} else {
		result, err = client.GenerateText(ctx, prompt)
	}
	if err != nil {
		return nil, s.writeChatCompletionsError(c, http.StatusBadGateway, "upstream_error", err.Error())
	}

	content := ""
	if isImage {
		content = geminiWebResultContentJSON(result, "image")
	} else if isVideo {
		content = geminiWebResultContentJSON(result, "video")
	} else if result != nil {
		content = result.Text
	}
	if strings.TrimSpace(content) == "" {
		return nil, s.writeChatCompletionsError(c, http.StatusBadGateway, "upstream_error", "Gemini Web response was empty")
	}

	promptTokens := estimateTextTokens(prompt)
	completionTokens := estimateTextTokens(content)
	if isImage || isVideo {
		completionTokens = 1
	}
	usage := &apicompat.ChatUsage{
		PromptTokens:     promptTokens,
		CompletionTokens: completionTokens,
		TotalTokens:      promptTokens + completionTokens,
	}
	contentJSON, _ := json.Marshal(content)
	resp := apicompat.ChatCompletionsResponse{
		ID:      "chatcmpl_" + randomHex(12),
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   req.Model,
		Choices: []apicompat.ChatChoice{
			{
				Index: 0,
				Message: apicompat.ChatMessage{
					Role:    "assistant",
					Content: contentJSON,
				},
				FinishReason: "stop",
			},
		},
		Usage: usage,
	}
	c.JSON(http.StatusOK, resp)

	return &ForwardResult{
		Usage: ClaudeUsage{
			InputTokens:  promptTokens,
			OutputTokens: completionTokens,
		},
		Model:           req.Model,
		UpstreamModel:   "gemini-web-official",
		Stream:          false,
		Duration:        time.Since(startTime),
		ReasoningEffort: extractCCReasoningEffortFromBody(originalBody),
		ImageCount:      geminiWebImageCount(isImage, result),
		ImageSize:       geminiWebImageSize(isImage, result),
		ImageInputSize:  "",
	}, nil
}

func geminiWebIsVideoRequest(model string) bool {
	model = strings.ToLower(strings.TrimSpace(model))
	return strings.Contains(model, "veo") || strings.Contains(model, "video")
}

func geminiWebImageCount(isImage bool, result *geminiWebGenerateResult) int {
	if !isImage || result == nil || (result.ImageURL == "" && result.ImageID == "") {
		return 0
	}
	return 1
}

func geminiWebImageSize(isImage bool, result *geminiWebGenerateResult) string {
	if geminiWebImageCount(isImage, result) == 0 {
		return ""
	}
	return "1K"
}

func geminiWebPromptFromChatCompletions(req *apicompat.ChatCompletionsRequest) (string, error) {
	if req == nil {
		return "", errors.New("request is nil")
	}
	var parts []string
	if strings.TrimSpace(req.Instructions) != "" {
		parts = append(parts, strings.TrimSpace(req.Instructions))
	}
	for _, msg := range req.Messages {
		text := geminiWebChatContentText(msg.Content)
		if strings.TrimSpace(text) == "" {
			continue
		}
		role := strings.TrimSpace(msg.Role)
		if role == "" {
			parts = append(parts, strings.TrimSpace(text))
			continue
		}
		parts = append(parts, fmt.Sprintf("%s: %s", role, strings.TrimSpace(text)))
	}
	if len(parts) == 0 {
		return "", errors.New("messages must contain text content")
	}
	return strings.Join(parts, "\n"), nil
}

func geminiWebChatContentText(raw json.RawMessage) string {
	if len(raw) == 0 || strings.EqualFold(strings.TrimSpace(string(raw)), "null") {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var parts []apicompat.ChatContentPart
	if err := json.Unmarshal(raw, &parts); err == nil {
		out := make([]string, 0, len(parts))
		for _, part := range parts {
			switch part.Type {
			case "text", "input_text", "":
				if strings.TrimSpace(part.Text) != "" {
					out = append(out, strings.TrimSpace(part.Text))
				}
			case "image_url":
				if part.ImageURL != nil && strings.TrimSpace(part.ImageURL.URL) != "" {
					out = append(out, "[image_url] "+strings.TrimSpace(part.ImageURL.URL))
				}
			}
		}
		return strings.Join(out, "\n")
	}
	var generic []map[string]any
	if err := json.Unmarshal(raw, &generic); err == nil {
		out := make([]string, 0, len(generic))
		for _, part := range generic {
			switch strings.TrimSpace(fmt.Sprint(part["type"])) {
			case "text", "input_text", "":
				if text := strings.TrimSpace(fmt.Sprint(part["text"])); text != "" && text != "<nil>" {
					out = append(out, text)
				}
			}
		}
		return strings.Join(out, "\n")
	}
	return ""
}
