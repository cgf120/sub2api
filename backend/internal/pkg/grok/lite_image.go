package grok

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
)

const assetsBaseURL = "https://assets.grok.com"

var errLiteImageComplete = errors.New("grok lite image generation complete")

func (c *Client) GenerateLiteImages(ctx context.Context, req ImageRequest) ([]ImageResult, error) {
	prompt := strings.TrimSpace(req.Prompt)
	if prompt == "" {
		return nil, errors.New("prompt is required")
	}
	count := req.Count
	if count <= 0 {
		count = 1
	}
	if count > 2 {
		count = 2
	}

	payload := buildChatPayload("Drawing: "+prompt, "fast")
	resp, err := c.postStream(ctx, ChatURL, BaseURL+"/", payload)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	seen := make(map[string]struct{})
	results := make([]ImageResult, 0, count)
	err = readStreamLines(ctx, resp.Body, func(data string) error {
		if msg := extractStreamError(data); msg != "" {
			return &UpstreamError{StatusCode: 502, Message: msg}
		}
		event, ok, err := extractLiteImageEvent(data)
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
		if event.Moderated {
			return &UpstreamError{StatusCode: 502, Message: "grok lite image generation was moderated"}
		}
		if event.Progress < 100 || event.URL == "" {
			return nil
		}
		id := event.ID
		if id == "" {
			id = event.URL
		}
		if _, exists := seen[id]; exists {
			return nil
		}
		seen[id] = struct{}{}
		results = append(results, ImageResult{
			ID:        event.ID,
			URL:       event.URL,
			Moderated: event.Moderated,
		})
		if len(results) >= count {
			return errLiteImageComplete
		}
		return nil
	})
	if errors.Is(err, errLiteImageComplete) {
		err = nil
	}
	if err != nil {
		return nil, err
	}
	if len(results) == 0 {
		return nil, &UpstreamError{StatusCode: 502, Message: "grok lite image stream ended without final imageUrl"}
	}
	return results, nil
}

type liteImageEvent struct {
	Progress  int
	URL       string
	ID        string
	Moderated bool
}

func extractLiteImageEvent(data string) (liteImageEvent, bool, error) {
	var obj struct {
		Result struct {
			Response struct {
				CardAttachment struct {
					JSONData string `json:"jsonData"`
				} `json:"cardAttachment"`
			} `json:"response"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(data), &obj); err != nil {
		return liteImageEvent{}, false, nil
	}
	raw := strings.TrimSpace(obj.Result.Response.CardAttachment.JSONData)
	if raw == "" {
		return liteImageEvent{}, false, nil
	}

	var payload struct {
		ImageChunk struct {
			Progress  int    `json:"progress"`
			ImageURL  string `json:"imageUrl"`
			ImageUUID string `json:"imageUuid"`
			Moderated bool   `json:"moderated"`
		} `json:"image_chunk"`
	}
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return liteImageEvent{}, false, err
	}
	url := strings.TrimSpace(payload.ImageChunk.ImageURL)
	if url == "" && strings.TrimSpace(payload.ImageChunk.ImageUUID) == "" {
		return liteImageEvent{}, false, nil
	}
	return liteImageEvent{
		Progress:  payload.ImageChunk.Progress,
		URL:       absolutizeLiteImageURL(url),
		ID:        strings.TrimSpace(payload.ImageChunk.ImageUUID),
		Moderated: payload.ImageChunk.Moderated,
	}, true, nil
}

func absolutizeLiteImageURL(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || strings.HasPrefix(value, "https://") || strings.HasPrefix(value, "http://") {
		return value
	}
	return assetsBaseURL + "/" + strings.TrimLeft(value, "/")
}
