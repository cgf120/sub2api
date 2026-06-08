package grok

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	MediaPostURL           = BaseURL + "/rest/media/post/create"
	MediaPostCreateLinkURL = BaseURL + "/rest/media/post/create-link"
	videoMediaType         = "MEDIA_POST_TYPE_VIDEO"
	videoModelName         = "imagine-video-gen"
	videoPublicBaseURL     = "https://imagine-public.x.ai/imagine-public/share-videos"
)

type VideoRequest struct {
	Prompt         string
	AspectRatio    string
	ResolutionName string
	Seconds        int
	Preset         string
}

type VideoResult struct {
	PostID       string `json:"id,omitempty"`
	AssetID      string `json:"asset_id,omitempty"`
	URL          string `json:"url,omitempty"`
	PublicURL    string `json:"public_url,omitempty"`
	DownloadURL  string `json:"download_url,omitempty"`
	PrivateURL   string `json:"private_url,omitempty"`
	ShareURL     string `json:"share_url,omitempty"`
	ThumbnailURL string `json:"thumbnail_url,omitempty"`
}

func (c *Client) GenerateVideo(ctx context.Context, req VideoRequest, onProgress func(int) error) (*VideoResult, error) {
	prompt := strings.TrimSpace(req.Prompt)
	if prompt == "" {
		return nil, errors.New("prompt is required")
	}
	seconds := req.Seconds
	if seconds <= 0 {
		seconds = 6
	}
	aspectRatio := strings.TrimSpace(req.AspectRatio)
	if aspectRatio == "" {
		aspectRatio = "9:16"
	}
	resolutionName := strings.TrimSpace(req.ResolutionName)
	if resolutionName == "" {
		resolutionName = "720p"
	}
	preset := strings.TrimSpace(strings.ToLower(req.Preset))
	if preset == "" {
		preset = "custom"
	}

	parentPostID, err := c.createVideoMediaPost(ctx, prompt)
	if err != nil {
		return nil, err
	}
	payload := buildVideoPayload(prompt, parentPostID, aspectRatio, resolutionName, seconds, preset)
	resp, err := c.postStream(ctx, ChatURL, BaseURL+"/imagine", payload)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	result := &VideoResult{}
	err = readStreamLines(ctx, resp.Body, func(data string) error {
		if msg := extractStreamError(data); msg != "" {
			return &UpstreamError{StatusCode: http.StatusBadGateway, Message: msg}
		}
		stream := extractVideoStream(data)
		if stream == nil {
			return nil
		}
		progress := toInt(stream["progress"])
		if onProgress != nil && progress > 0 {
			if err := onProgress(progress); err != nil {
				return err
			}
		}
		if postID := strings.TrimSpace(fmt.Sprint(firstNonEmpty(stream["videoPostId"], stream["videoId"]))); postID != "" {
			result.PostID = postID
		}
		if progress >= 100 && !toBool(stream["moderated"]) {
			if rawURL := strings.TrimSpace(fmt.Sprint(stream["videoUrl"])); rawURL != "" && rawURL != "<nil>" {
				result.PrivateURL = absolutizeAssetURL(rawURL)
			}
			if assetID := strings.TrimSpace(fmt.Sprint(stream["assetId"])); assetID != "" && assetID != "<nil>" {
				result.AssetID = assetID
			}
			if thumb := strings.TrimSpace(fmt.Sprint(stream["thumbnailImageUrl"])); thumb != "" && thumb != "<nil>" {
				result.ThumbnailURL = absolutizeAssetURL(thumb)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if result.PostID == "" && result.AssetID != "" {
		result.PostID = result.AssetID
	}
	if result.PostID == "" {
		return nil, errors.New("grok video generation returned no post id")
	}
	result.ShareURL = videoShareURL(result.PostID)
	publicURL, err := c.createVideoPublicLink(ctx, result.PostID)
	if err != nil {
		return nil, err
	}
	result.PublicURL = publicURL
	result.URL = result.PublicURL
	result.DownloadURL = downloadVideoURLFromPublicURL(result.PostID, result.PublicURL)
	return result, nil
}

func (c *Client) createVideoMediaPost(ctx context.Context, prompt string) (string, error) {
	var out struct {
		Post struct {
			ID string `json:"id"`
		} `json:"post"`
	}
	if err := c.postJSON(ctx, MediaPostURL, BaseURL+"/imagine", map[string]any{
		"mediaType": videoMediaType,
		"prompt":    prompt,
	}, &out); err != nil {
		return "", err
	}
	if strings.TrimSpace(out.Post.ID) == "" {
		return "", errors.New("grok create media post returned no post id")
	}
	return strings.TrimSpace(out.Post.ID), nil
}

func (c *Client) createVideoPublicLink(ctx context.Context, postID string) (string, error) {
	postID = strings.TrimSpace(postID)
	if postID == "" {
		return "", errors.New("grok create video public link requires post id")
	}
	var out struct {
		ShareLink string `json:"shareLink"`
	}
	payload := map[string]any{
		"postId":   postID,
		"source":   "post-page",
		"platform": "web",
	}
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			if err := sleepContext(ctx, time.Duration(attempt)*500*time.Millisecond); err != nil {
				return "", err
			}
		}
		out.ShareLink = ""
		if err := c.postJSON(ctx, MediaPostCreateLinkURL, BaseURL, payload, &out); err != nil {
			lastErr = err
			continue
		}
		shareLink := strings.TrimSpace(out.ShareLink)
		if shareLink == "" {
			lastErr = errors.New("grok create video public link returned no share link")
			continue
		}
		return publicVideoURLFromShareLink(postID, shareLink), nil
	}
	return "", fmt.Errorf("grok create video public link failed: %w", lastErr)
}

func buildVideoPayload(prompt, parentPostID, aspectRatio, resolutionName string, seconds int, preset string) map[string]any {
	return map[string]any{
		"temporary":        true,
		"modelName":        videoModelName,
		"message":          buildVideoMessage(prompt, preset),
		"enableSideBySide": true,
		"responseMetadata": map[string]any{
			"experiments": []any{},
			"modelConfigOverride": map[string]any{
				"modelMap": map[string]any{
					"videoGenModelConfig": map[string]any{
						"parentPostId":   parentPostID,
						"aspectRatio":    aspectRatio,
						"videoLength":    seconds,
						"resolutionName": resolutionName,
					},
				},
			},
		},
	}
}

func buildVideoMessage(prompt, preset string) string {
	flags := map[string]string{
		"fun":    "--mode=extremely-crazy",
		"normal": "--mode=normal",
		"spicy":  "--mode=extremely-spicy-or-crazy",
		"custom": "--mode=custom",
	}
	flag := flags[preset]
	if flag == "" {
		flag = flags["custom"]
	}
	return strings.TrimSpace(prompt + " " + flag)
}

func extractVideoStream(data string) map[string]any {
	var obj map[string]any
	if err := json.Unmarshal([]byte(data), &obj); err != nil {
		return nil
	}
	result, _ := obj["result"].(map[string]any)
	response, _ := result["response"].(map[string]any)
	stream, _ := response["streamingVideoGenerationResponse"].(map[string]any)
	return stream
}

func absolutizeAssetURL(value string) string {
	if strings.HasPrefix(value, "https://") {
		return value
	}
	if strings.HasPrefix(value, "/") {
		return "https://assets.grok.com" + value
	}
	return "https://assets.grok.com/" + value
}

func publicVideoURL(postID string) string {
	return videoPublicBaseURL + "/" + strings.TrimSpace(postID) + ".mp4?dl=0"
}

func downloadVideoURL(postID string) string {
	return videoPublicBaseURL + "/" + strings.TrimSpace(postID) + ".mp4?dl=1"
}

func publicVideoURLFromShareLink(postID, shareLink string) string {
	shareLink = strings.TrimSpace(shareLink)
	if shareLink == "" || !isMP4URL(shareLink) {
		return publicVideoURL(postID)
	}
	if isPublicVideoAssetURL(shareLink) {
		return videoURLWithDownloadMode(shareLink, false)
	}
	return shareLink
}

func downloadVideoURLFromPublicURL(postID, publicURL string) string {
	if isPublicVideoAssetURL(publicURL) {
		return videoURLWithDownloadMode(publicURL, true)
	}
	return downloadVideoURL(postID)
}

func isMP4URL(raw string) bool {
	value := strings.TrimSpace(raw)
	if idx := strings.IndexAny(value, "?#"); idx >= 0 {
		value = value[:idx]
	}
	return strings.HasSuffix(strings.ToLower(value), ".mp4")
}

func isPublicVideoAssetURL(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return false
	}
	return strings.EqualFold(u.Host, "imagine-public.x.ai") &&
		strings.HasPrefix(u.Path, "/imagine-public/share-videos/")
}

func videoURLWithDownloadMode(raw string, download bool) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return raw
	}
	q := u.Query()
	q.Del("cache")
	if download {
		q.Set("dl", "1")
	} else {
		q.Set("dl", "0")
	}
	u.RawQuery = q.Encode()
	return u.String()
}

func videoShareURL(postID string) string {
	return BaseURL + "/imagine/post/" + strings.TrimSpace(postID) + "?source=post-page&platform=web"
}

func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
