package grok

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	coderws "github.com/coder/websocket"
	"github.com/google/uuid"
)

const (
	WSImagineURL      = "wss://grok.com/ws/imagine/listen"
	imaginePublicBase = "https://imagine-public.x.ai/imagine-public/images"
)

var imageURLPattern = regexp.MustCompile(`/images/([a-f0-9\-]+)\.(png|jpg|jpeg)`)

type ImageRequest struct {
	Prompt      string
	Count       int
	AspectRatio string
	EnableNSFW  bool
	EnablePro   bool
}

type ImageResult struct {
	ID        string `json:"id,omitempty"`
	URL       string `json:"url"`
	Width     int    `json:"width,omitempty"`
	Height    int    `json:"height,omitempty"`
	RRated    bool   `json:"r_rated,omitempty"`
	Moderated bool   `json:"moderated,omitempty"`
}

type imagineSlot struct {
	id       string
	order    int
	width    int
	height   int
	lastURL  string
	done     bool
	rRated   bool
	progress int
}

func (c *Client) GenerateImages(ctx context.Context, req ImageRequest) ([]ImageResult, error) {
	prompt := strings.TrimSpace(req.Prompt)
	if prompt == "" {
		return nil, errors.New("prompt is required")
	}
	count := req.Count
	if count <= 0 {
		count = 1
	}
	if count > 10 {
		count = 10
	}
	aspectRatio := strings.TrimSpace(req.AspectRatio)
	if aspectRatio == "" {
		aspectRatio = "2:3"
	}

	headers := c.Headers("", BaseURL+"/imagine")
	headers.Set("Content-Type", "")
	headers.Set("Accept", "*/*")
	opts := &coderws.DialOptions{
		HTTPHeader:      headers,
		CompressionMode: coderws.CompressionContextTakeover,
	}
	if c.cfg.ProxyURL != "" {
		opts.HTTPClient = c.httpClient
	}
	conn, resp, err := coderws.Dial(ctx, WSImagineURL, opts)
	if err != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		return nil, &UpstreamError{StatusCode: status, Message: err.Error()}
	}
	defer func() {
		_ = conn.Close(coderws.StatusNormalClosure, "")
	}()
	conn.SetReadLimit(16 * 1024 * 1024)

	if err := c.writeWSJSON(ctx, conn, buildImagineResetMessage()); err != nil {
		return nil, err
	}
	if err := c.writeWSJSON(ctx, conn, buildImagineRequestMessage(uuid.NewString(), prompt, aspectRatio, req.EnableNSFW, req.EnablePro)); err != nil {
		return nil, err
	}

	slots := map[string]*imagineSlot{}
	results := make([]ImageResult, 0, count)
	for len(results) < count {
		messageType, data, err := conn.Read(ctx)
		if err != nil {
			if len(results) > 0 {
				return results, nil
			}
			return nil, err
		}
		if messageType != coderws.MessageText {
			continue
		}
		var frame map[string]any
		if err := json.Unmarshal(data, &frame); err != nil {
			continue
		}
		switch frameType := strings.TrimSpace(fmt.Sprint(frame["type"])); frameType {
		case "json":
			result, ok, err := handleImagineJSONFrame(frame, slots)
			if err != nil {
				return nil, err
			}
			if ok {
				results = append(results, result)
			}
		case "image":
			handleImagineImageFrame(frame, slots)
		case "error":
			code := strings.TrimSpace(fmt.Sprint(frame["err_code"]))
			msg := strings.TrimSpace(fmt.Sprint(frame["err_msg"]))
			if msg == "" {
				msg = string(data)
			}
			if code != "" {
				msg = code + ": " + msg
			}
			return nil, &UpstreamError{StatusCode: http.StatusBadGateway, Message: msg}
		}
	}
	return results, nil
}

func (c *Client) writeWSJSON(ctx context.Context, conn *coderws.Conn, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return conn.Write(ctx, coderws.MessageText, data)
}

func buildImagineResetMessage() map[string]any {
	return map[string]any{
		"type":      "conversation.item.create",
		"timestamp": time.Now().UnixMilli(),
		"item": map[string]any{
			"type": "message",
			"content": []any{
				map[string]any{"type": "reset"},
			},
		},
	}
}

func buildImagineRequestMessage(requestID, prompt, aspectRatio string, enableNSFW, enablePro bool) map[string]any {
	return map[string]any{
		"type":      "conversation.item.create",
		"timestamp": time.Now().UnixMilli(),
		"item": map[string]any{
			"type": "message",
			"content": []any{
				map[string]any{
					"requestId": requestID,
					"text":      prompt,
					"type":      "input_text",
					"properties": map[string]any{
						"section_count":       0,
						"is_kids_mode":        false,
						"enable_nsfw":         enableNSFW,
						"skip_upsampler":      false,
						"enable_side_by_side": true,
						"is_initial":          false,
						"aspect_ratio":        aspectRatio,
						"enable_pro":          enablePro,
					},
				},
			},
		},
	}
}

func handleImagineJSONFrame(frame map[string]any, slots map[string]*imagineSlot) (ImageResult, bool, error) {
	status := strings.TrimSpace(fmt.Sprint(frame["current_status"]))
	if status != "start_stage" && status != "completed" {
		return ImageResult{}, false, nil
	}
	imageID := strings.TrimSpace(fmt.Sprint(firstNonEmpty(frame["image_id"], frame["job_id"])))
	if imageID == "" {
		return ImageResult{}, false, nil
	}
	slot := slots[imageID]
	if slot == nil {
		slot = &imagineSlot{id: imageID}
		slots[imageID] = slot
	}
	if status == "start_stage" {
		slot.order = toInt(frame["order"])
		slot.width = toInt(frame["width"])
		slot.height = toInt(frame["height"])
		return ImageResult{}, false, nil
	}
	if slot.done {
		return ImageResult{}, false, nil
	}
	slot.done = true
	slot.rRated = toBool(frame["r_rated"])
	if toBool(frame["moderated"]) {
		return ImageResult{}, false, &UpstreamError{StatusCode: http.StatusBadGateway, Message: "grok image generation was moderated"}
	}
	return ImageResult{
		ID:     imageID,
		URL:    publicImageURL(imageID),
		Width:  slot.width,
		Height: slot.height,
		RRated: slot.rRated,
	}, true, nil
}

func handleImagineImageFrame(frame map[string]any, slots map[string]*imagineSlot) {
	urlValue := strings.TrimSpace(fmt.Sprint(frame["url"]))
	imageID := parseImageIDFromURL(urlValue)
	if imageID == "" {
		return
	}
	slot := slots[imageID]
	if slot == nil {
		slot = &imagineSlot{id: imageID}
		slots[imageID] = slot
	}
	slot.lastURL = urlValue
	if progress := toInt(frame["percentage_complete"]); progress > slot.progress {
		slot.progress = progress
	}
}

func parseImageIDFromURL(value string) string {
	matches := imageURLPattern.FindStringSubmatch(value)
	if len(matches) >= 2 {
		return matches[1]
	}
	return ""
}

func publicImageURL(imageID string) string {
	return imaginePublicBase + "/" + strings.TrimSpace(imageID) + ".jpg"
}

func firstNonEmpty(values ...any) any {
	for _, v := range values {
		if strings.TrimSpace(fmt.Sprint(v)) != "" && fmt.Sprint(v) != "<nil>" {
			return v
		}
	}
	return ""
}

func toBool(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case string:
		parsed, _ := strconv.ParseBool(t)
		return parsed
	default:
		return false
	}
}

func toInt(v any) int {
	switch t := v.(type) {
	case int:
		return t
	case int64:
		return int(t)
	case float64:
		return int(t)
	case json.Number:
		i, _ := t.Int64()
		return int(i)
	case string:
		i, _ := strconv.Atoi(strings.TrimSpace(t))
		return i
	default:
		return 0
	}
}
