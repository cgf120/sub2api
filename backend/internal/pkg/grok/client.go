package grok

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/proxyurl"
	"github.com/Wei-Shaw/sub2api/internal/pkg/proxyutil"
	"github.com/google/uuid"
)

const (
	BaseURL       = "https://grok.com"
	ChatURL       = BaseURL + "/rest/app-chat/conversations/new"
	RateLimitsURL = BaseURL + "/rest/rate-limits"

	defaultUserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/136.0.0.0 Safari/537.36"
	statsigID        = "eDE6VHlwZUVycm9yOiBDYW5ub3QgcmVhZCBwcm9wZXJ0aWVzIG9mIHVuZGVmaW5lZA=="
)

type Config struct {
	SSO          string
	UserAgent    string
	ExtraCookies string
	ProxyURL     string
}

type Client struct {
	cfg        Config
	httpClient *http.Client
}

func NewClient(cfg Config) (*Client, error) {
	cfg.SSO = cleanSSO(cfg.SSO)
	cfg.UserAgent = strings.TrimSpace(cfg.UserAgent)
	cfg.ExtraCookies = strings.TrimSpace(strings.Trim(cfg.ExtraCookies, "; "))
	cfg.ProxyURL = strings.TrimSpace(cfg.ProxyURL)
	if cfg.SSO == "" {
		return nil, errors.New("grok sso credential is empty")
	}
	if cfg.UserAgent == "" {
		cfg.UserAgent = defaultUserAgent
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	if cfg.ProxyURL != "" {
		trimmed, parsed, err := proxyurl.Parse(cfg.ProxyURL)
		if err != nil {
			return nil, fmt.Errorf("invalid proxy url: %w", err)
		}
		cfg.ProxyURL = trimmed
		if err := proxyutil.ConfigureTransportProxy(transport, parsed); err != nil {
			return nil, err
		}
	}

	return &Client{
		cfg: cfg,
		httpClient: &http.Client{
			Transport: transport,
			Timeout:   0,
		},
	}, nil
}

func cleanSSO(raw string) string {
	token := strings.TrimSpace(raw)
	token = strings.TrimPrefix(token, "sso=")
	return strings.Join(strings.Fields(token), "")
}

func (c *Client) CookieHeader() string {
	cookie := "sso=" + c.cfg.SSO + "; sso-rw=" + c.cfg.SSO
	if c.cfg.ExtraCookies != "" {
		cookie += "; " + c.cfg.ExtraCookies
	}
	return cookie
}

func (c *Client) HTTPClient() *http.Client {
	return c.httpClient
}

func (c *Client) ProxyURL() string {
	return c.cfg.ProxyURL
}

func (c *Client) Headers(contentType, referer string) http.Header {
	if referer == "" {
		referer = BaseURL + "/"
	}
	headers := http.Header{}
	headers.Set("Accept", "*/*")
	headers.Set("Accept-Encoding", "identity")
	headers.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	headers.Set("Cookie", c.CookieHeader())
	headers.Set("Origin", BaseURL)
	headers.Set("Referer", referer)
	headers.Set("Sec-Fetch-Dest", "empty")
	headers.Set("Sec-Fetch-Mode", "cors")
	headers.Set("Sec-Fetch-Site", "same-origin")
	headers.Set("User-Agent", c.cfg.UserAgent)
	headers.Set("x-statsig-id", statsigID)
	headers.Set("x-xai-request-id", uuid.NewString())
	if contentType != "" {
		headers.Set("Content-Type", contentType)
	}
	return headers
}

func (c *Client) postJSON(ctx context.Context, targetURL, referer string, payload any, out any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header = c.Headers("application/json", referer)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return readStatusError(resp)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (c *Client) postStream(ctx context.Context, targetURL, referer string, payload any) (*http.Response, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header = c.Headers("application/json", referer)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer func() { _ = resp.Body.Close() }()
		return nil, readStatusError(resp)
	}
	return resp, nil
}

func (c *Client) get(ctx context.Context, targetURL, referer string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header = c.Headers("", referer)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer func() { _ = resp.Body.Close() }()
		return nil, readStatusError(resp)
	}
	return resp, nil
}

func readStatusError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	msg := strings.TrimSpace(string(body))
	if msg == "" {
		msg = resp.Status
	}
	return &UpstreamError{StatusCode: resp.StatusCode, Message: msg}
}

type UpstreamError struct {
	StatusCode int
	Message    string
}

func (e *UpstreamError) Error() string {
	if e == nil {
		return ""
	}
	if e.StatusCode > 0 {
		return fmt.Sprintf("grok upstream returned %d: %s", e.StatusCode, e.Message)
	}
	return e.Message
}

func IsAntiBotError(err error) bool {
	var upstreamErr *UpstreamError
	if !errors.As(err, &upstreamErr) {
		return false
	}
	message := strings.ToLower(upstreamErr.Message)
	return upstreamErr.StatusCode == http.StatusForbidden &&
		(strings.Contains(message, `"code":7`) ||
			strings.Contains(message, "anti-bot") ||
			strings.Contains(message, "request rejected by anti-bot"))
}

func IsInvalidSessionError(err error) bool {
	var upstreamErr *UpstreamError
	if !errors.As(err, &upstreamErr) {
		return false
	}
	message := strings.ToLower(upstreamErr.Message)
	if upstreamErr.StatusCode == http.StatusUnauthorized {
		return true
	}
	return strings.Contains(message, "invalid-credentials") ||
		strings.Contains(message, "bad-credentials") ||
		strings.Contains(message, "failed to look up session id") ||
		strings.Contains(message, "session not found") ||
		strings.Contains(message, "token revoked") ||
		strings.Contains(message, "token expired")
}

func IsRateLimitError(err error) bool {
	var upstreamErr *UpstreamError
	if errors.As(err, &upstreamErr) && upstreamErr.StatusCode == http.StatusTooManyRequests {
		return true
	}
	message := strings.ToLower(errString(err))
	return strings.Contains(message, "rate_limit_exceeded") ||
		strings.Contains(message, "rate limit exceeded") ||
		strings.Contains(message, "too many requests")
}

func IsTransientDisconnectError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "failed to get reader") ||
		strings.Contains(message, "failed to read frame header") ||
		strings.Contains(message, "unexpected eof") ||
		strings.Contains(message, "connection reset by peer") ||
		strings.Contains(message, "use of closed network connection")
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func readStreamLines(ctx context.Context, r io.Reader, onData func(string) error) error {
	reader := bufio.NewReader(r)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		line, err := reader.ReadString('\n')
		if len(line) > 0 {
			eventType, data := classifyLine(line)
			switch eventType {
			case "done":
				return nil
			case "data":
				if err := onData(data); err != nil {
					return err
				}
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

func classifyLine(line string) (eventType, data string) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "event:") {
		return "skip", ""
	}
	if strings.HasPrefix(line, "data:") {
		data = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			return "done", ""
		}
		return "data", data
	}
	if strings.HasPrefix(line, "{") {
		return "data", line
	}
	return "skip", ""
}

func defaultDeviceEnvInfo() map[string]any {
	return map[string]any{
		"darkModeEnabled":  false,
		"devicePixelRatio": 2,
		"screenHeight":     1329,
		"screenWidth":      2056,
		"viewportHeight":   1083,
		"viewportWidth":    2056,
	}
}

func defaultToolOverrides() map[string]bool {
	return map[string]bool{
		"gmailSearch":           false,
		"googleCalendarSearch":  false,
		"outlookSearch":         false,
		"outlookCalendarSearch": false,
		"googleDriveSearch":     false,
	}
}

func buildChatPayload(message, modeID string) map[string]any {
	if strings.TrimSpace(modeID) == "" {
		modeID = "fast"
	}
	return map[string]any{
		"collectionIds":               []any{},
		"connectors":                  []any{},
		"deviceEnvInfo":               defaultDeviceEnvInfo(),
		"disableMemory":               true,
		"disableSearch":               false,
		"disableSelfHarmShortCircuit": false,
		"disableTextFollowUps":        false,
		"enableImageGeneration":       true,
		"enableImageStreaming":        true,
		"enableSideBySide":            true,
		"fileAttachments":             []any{},
		"forceConcise":                false,
		"forceSideBySide":             false,
		"imageAttachments":            []any{},
		"imageGenerationCount":        2,
		"isAsyncChat":                 false,
		"message":                     message,
		"modeId":                      modeID,
		"responseMetadata":            map[string]any{},
		"returnImageBytes":            false,
		"returnRawGrokInXaiRequest":   false,
		"searchAllConnectors":         false,
		"sendFinalMetadata":           true,
		"temporary":                   true,
		"toolOverrides":               defaultToolOverrides(),
	}
}

func extractStreamError(data string) string {
	var obj map[string]any
	if err := json.Unmarshal([]byte(data), &obj); err != nil {
		return ""
	}
	errObj, _ := obj["error"].(map[string]any)
	if errObj == nil {
		return ""
	}
	for _, key := range []string{"message", "error"} {
		if v, ok := errObj[key].(string); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	if len(errObj) > 0 {
		raw, _ := json.Marshal(errObj)
		return string(raw)
	}
	return ""
}
