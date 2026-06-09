package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	geminiWebOAuthType         = "web"
	geminiWebInitURL           = "https://gemini.google.com/app"
	geminiWebGenerateURL       = "https://gemini.google.com/_/BardChatUi/data/assistant.lamda.BardFrontendService/StreamGenerate"
	geminiWebBatchExecuteURL   = "https://gemini.google.com/_/BardChatUi/data/batchexecute"
	geminiWebDefaultLanguage   = "en"
	geminiWebDefaultPushID     = "feeds/mcudyrk2a4khkz"
	geminiWebDefaultVideoWait  = 10 * time.Minute
	geminiWebPollInterval      = 10 * time.Second
	geminiWebMaxResponseBytes  = 32 << 20
	geminiWebCookieHeaderLimit = 32 << 10
)

var (
	geminiWebFrameLengthLine = regexp.MustCompile(`^\d+$`)
	geminiWebArtifactPattern = regexp.MustCompile(`http://googleusercontent\.com/\w+/\d+\n*`)
	geminiWebCookieNames     = []string{"__Secure-1PAPISID", "__Secure-1PSID", "__Secure-1PSIDTS", "__Secure-1PSIDCC", "__Secure-3PSID", "__Secure-3PSIDTS", "__Secure-3PSIDCC", "COMPASS", "NID"}
)

type geminiWebSession struct {
	AccessToken string
	BuildLabel  string
	SessionID   string
	Language    string
	PushID      string
	ReqID       int
}

type geminiWebOfficialClient struct {
	account      *Account
	httpUpstream HTTPUpstream
	cookies      map[string]string
	cookieHeader string
	session      *geminiWebSession
}

type geminiWebGenerateResult struct {
	Text         string `json:"text,omitempty"`
	ImageURL     string `json:"image_url,omitempty"`
	ImageID      string `json:"image_id,omitempty"`
	VideoURL     string `json:"url,omitempty"`
	ThumbnailURL string `json:"thumbnail_url,omitempty"`
	CID          string `json:"cid,omitempty"`
	RID          string `json:"rid,omitempty"`
	RCID         string `json:"rcid,omitempty"`
}

func (a *Account) IsGeminiWebOAuth() bool {
	return a != nil && a.Platform == PlatformGemini && a.Type == AccountTypeOAuth && strings.EqualFold(a.GeminiOAuthType(), geminiWebOAuthType)
}

func geminiWebAccountCanGenerateVideo(account *Account) bool {
	if account == nil || !account.IsGeminiWebOAuth() {
		return true
	}
	tierID := canonicalGeminiTierID(account.GeminiTierID())
	switch tierID {
	case "", GeminiTierGoogleOneFree, GeminiTierGoogleOneUnknown:
		return false
	default:
		return true
	}
}

func geminiWebVideoTierError(account *Account) error {
	tierID := canonicalGeminiTierID(account.GeminiTierID())
	if tierID == "" {
		tierID = GeminiTierGoogleOneFree
	}
	return fmt.Errorf("Gemini Web video generation is not available for tier %s", tierID)
}

func newGeminiWebOfficialClient(account *Account, upstream HTTPUpstream) (*geminiWebOfficialClient, error) {
	if account == nil {
		return nil, errors.New("account is nil")
	}
	if !account.IsGeminiWebOAuth() {
		return nil, errors.New("account is not Gemini Web OAuth")
	}
	if upstream == nil {
		return nil, errors.New("http upstream not configured")
	}
	if !geminiWebCookieInputIsJSON(account.Credentials) {
		return nil, errors.New("Gemini Web Cookie accounts require JSON cookie credentials; PSID::NID and raw Cookie headers are not supported")
	}
	cookies := geminiWebCookiesFromCredentials(account.Credentials)
	if strings.TrimSpace(cookies["__Secure-1PSID"]) == "" || strings.TrimSpace(cookies["NID"]) == "" {
		return nil, errors.New("Gemini Web JSON cookies require __Secure-1PSID and NID")
	}
	header := geminiWebCookieHeader(cookies)
	if header == "" || len(header) > geminiWebCookieHeaderLimit {
		return nil, errors.New("invalid Gemini Web cookie header")
	}
	return &geminiWebOfficialClient{
		account:      account,
		httpUpstream: upstream,
		cookies:      cookies,
		cookieHeader: header,
	}, nil
}

func geminiWebCookiesFromCredentials(credentials map[string]any) map[string]string {
	out := make(map[string]string)
	if credentials == nil {
		return out
	}

	mergeCookieMap(out, credentials["cookies"])
	mergeCookieMap(out, credentials["cookie"])

	for _, key := range geminiWebCookieNames {
		if v := credentialString(credentials, key); v != "" {
			out[key] = normalizeCookieValue(key, v)
		}
	}
	if v := credentialString(credentials, "secure_1psid"); v != "" {
		out["__Secure-1PSID"] = normalizeCookieValue("__Secure-1PSID", v)
	}
	if v := credentialString(credentials, "nid"); v != "" {
		out["NID"] = normalizeCookieValue("NID", v)
	}
	return out
}

func geminiWebCookieInputIsJSON(credentials map[string]any) bool {
	if credentials == nil {
		return false
	}
	return geminiWebCookieValueIsJSON(credentials["cookies"]) || geminiWebCookieValueIsJSON(credentials["cookie"])
}

func geminiWebCookieValueIsJSON(raw any) bool {
	switch v := raw.(type) {
	case map[string]any, map[string]string:
		return true
	case string:
		v = strings.TrimSpace(v)
		if !strings.HasPrefix(v, "{") {
			return false
		}
		var parsed map[string]any
		return json.Unmarshal([]byte(v), &parsed) == nil
	default:
		return false
	}
}

func isGeminiWebCookieName(name string) bool {
	for _, key := range geminiWebCookieNames {
		if name == key {
			return true
		}
	}
	return false
}

func credentialString(credentials map[string]any, key string) string {
	if credentials == nil {
		return ""
	}
	v, ok := credentials[key]
	if !ok || v == nil {
		return ""
	}
	switch x := v.(type) {
	case string:
		return strings.TrimSpace(x)
	case fmt.Stringer:
		return strings.TrimSpace(x.String())
	default:
		return ""
	}
}

func mergeCookieMap(out map[string]string, raw any) {
	switch v := raw.(type) {
	case nil:
		return
	case map[string]any:
		for k, val := range v {
			if strings.TrimSpace(k) == "cookies" {
				mergeCookieMap(out, val)
				continue
			}
			if !isGeminiWebCookieName(strings.TrimSpace(k)) {
				continue
			}
			s := strings.TrimSpace(fmt.Sprint(val))
			if strings.TrimSpace(k) != "" && s != "" {
				out[strings.TrimSpace(k)] = normalizeCookieValue(k, s)
			}
		}
	case map[string]string:
		for k, s := range v {
			if !isGeminiWebCookieName(strings.TrimSpace(k)) {
				continue
			}
			if strings.TrimSpace(k) != "" && strings.TrimSpace(s) != "" {
				out[strings.TrimSpace(k)] = normalizeCookieValue(k, s)
			}
		}
	case string:
		mergeCookieString(out, v)
	}
}

func mergeCookieString(out map[string]string, raw string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return
	}
	if !strings.HasPrefix(raw, "{") {
		return
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err == nil {
		mergeCookieMap(out, m)
	}
}

func splitCookiePart(part string) (string, string) {
	if idx := strings.Index(part, ":"); idx > 0 && !strings.Contains(part[:idx], "=") {
		return strings.TrimSpace(part[:idx]), strings.TrimSpace(part[idx+1:])
	}
	if idx := strings.Index(part, "="); idx > 0 {
		name := strings.TrimSpace(part[:idx])
		value := strings.TrimSpace(part[idx+1:])
		if fields := strings.Fields(name); len(fields) > 1 {
			return fields[0], strings.TrimSpace(strings.TrimPrefix(part, fields[0]))
		}
		return name, value
	}
	fields := strings.Fields(part)
	if len(fields) >= 2 {
		return fields[0], strings.TrimSpace(strings.TrimPrefix(part, fields[0]))
	}
	return "", ""
}

func normalizeCookieValue(name, value string) string {
	name = strings.TrimSpace(name)
	value = strings.TrimSpace(value)
	value = strings.Trim(value, "\"'")
	if strings.HasPrefix(value, name+"=") {
		value = strings.TrimSpace(strings.TrimPrefix(value, name+"="))
	}
	if strings.HasPrefix(value, name+":") {
		value = strings.TrimSpace(strings.TrimPrefix(value, name+":"))
	}
	if strings.HasPrefix(value, name+" ") {
		value = strings.TrimSpace(strings.TrimPrefix(value, name+" "))
	}
	return value
}

func geminiWebCookieHeader(cookies map[string]string) string {
	if len(cookies) == 0 {
		return ""
	}
	seen := make(map[string]struct{}, len(cookies))
	parts := make([]string, 0, len(cookies))
	for _, key := range geminiWebCookieNames {
		if v := strings.TrimSpace(cookies[key]); v != "" {
			parts = append(parts, key+"="+v)
			seen[key] = struct{}{}
		}
	}
	var rest []string
	for key, value := range cookies {
		if _, ok := seen[key]; ok {
			continue
		}
		if strings.TrimSpace(key) != "" && strings.TrimSpace(value) != "" {
			rest = append(rest, key)
		}
	}
	sort.Strings(rest)
	for _, key := range rest {
		parts = append(parts, key+"="+strings.TrimSpace(cookies[key]))
	}
	return strings.Join(parts, "; ")
}

func (g *geminiWebOfficialClient) Init(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, geminiWebInitURL, nil)
	if err != nil {
		return err
	}
	g.addCommonHeaders(req)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")

	status, headers, body, err := g.do(req)
	if err != nil {
		return err
	}
	if status >= 400 {
		return fmt.Errorf("Gemini Web init returned status %d", status)
	}
	g.mergeSetCookies(headers)
	html := string(body)
	session := &geminiWebSession{
		AccessToken: extractGoogleBootValue(html, "SNlM0e"),
		BuildLabel:  extractGoogleBootValue(html, "cfb2h"),
		SessionID:   extractGoogleBootValue(html, "FdrFJe"),
		Language:    extractGoogleBootValue(html, "TuX5cc"),
		PushID:      extractGoogleBootValue(html, "qKIAYe"),
		ReqID:       10000 + int(time.Now().UnixNano()%89999),
	}
	if session.Language == "" {
		session.Language = geminiWebDefaultLanguage
	}
	if session.PushID == "" {
		session.PushID = geminiWebDefaultPushID
	}
	if session.AccessToken == "" || session.BuildLabel == "" || session.SessionID == "" {
		return errors.New("Gemini Web init failed: missing SNlM0e, cfb2h, or FdrFJe")
	}
	g.session = session
	return nil
}

func (g *geminiWebOfficialClient) GenerateText(ctx context.Context, prompt string) (*geminiWebGenerateResult, error) {
	return g.generate(ctx, prompt, false)
}

func (g *geminiWebOfficialClient) GenerateImage(ctx context.Context, prompt string) (*geminiWebGenerateResult, error) {
	result, err := g.generate(ctx, prompt, false)
	if err != nil {
		return nil, err
	}
	if result != nil && result.ImageURL == "" && result.CID != "" {
		if polled, pollErr := g.pollGeneratedImage(ctx, result.CID, 90*time.Second); pollErr == nil && polled != nil && polled.ImageURL != "" {
			result = polled
		}
	}
	if result != nil && result.ImageURL == "" && result.ImageID != "" {
		if resolved, resolveErr := g.resolveGeneratedImageURL(ctx, result); resolveErr == nil && resolved != "" {
			result.ImageURL = resolved
		}
	}
	if result == nil || (result.ImageURL == "" && result.ImageID == "") {
		return result, errors.New("Gemini Web image generation did not return an image")
	}
	return result, nil
}

func (g *geminiWebOfficialClient) GenerateVideo(ctx context.Context, prompt string) (*geminiWebGenerateResult, error) {
	if !geminiWebAccountCanGenerateVideo(g.account) {
		return nil, geminiWebVideoTierError(g.account)
	}
	result, err := g.generate(ctx, prompt, true)
	if err != nil {
		return nil, err
	}
	if result != nil && result.VideoURL != "" {
		return result, nil
	}
	if result == nil || result.CID == "" {
		return result, errors.New("Gemini Web video generation started but no conversation id was returned")
	}
	timeout := geminiWebDefaultVideoWait
	if seconds, err := strconv.Atoi(strings.TrimSpace(g.account.GetCredential("video_timeout_seconds"))); err == nil && seconds > 0 {
		timeout = time.Duration(seconds) * time.Second
	}
	return g.pollGeneratedVideo(ctx, result.CID, timeout)
}

func (g *geminiWebOfficialClient) generate(ctx context.Context, prompt string, video bool) (*geminiWebGenerateResult, error) {
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return nil, errors.New("prompt is required")
	}
	if g.session == nil {
		if err := g.Init(ctx); err != nil {
			return nil, err
		}
	}

	inner := make([]any, 69)
	inner[0] = []any{prompt, 0, nil, nil, nil, nil, 0}
	inner[1] = []any{g.session.Language}
	inner[2] = []any{"", "", "", nil, nil, nil, nil, nil, nil, ""}
	inner[6] = []any{1}
	inner[7] = 1
	inner[10] = 1
	inner[11] = 0
	inner[17] = []any{[]any{0}}
	inner[18] = 0
	inner[27] = 1
	inner[30] = []any{4}
	inner[41] = []any{1}
	if video {
		inner[49] = 11
	}
	inner[53] = 0
	inner[61] = []any{}
	inner[68] = 2

	uuidVal := strings.ToUpper(uuid.NewString())
	inner[59] = uuidVal
	innerBytes, err := json.Marshal(inner)
	if err != nil {
		return nil, err
	}
	fReqBytes, err := json.Marshal([]any{nil, string(innerBytes)})
	if err != nil {
		return nil, err
	}

	params := url.Values{}
	params.Set("hl", g.session.Language)
	params.Set("_reqid", strconv.Itoa(g.nextReqID()))
	params.Set("rt", "c")
	params.Set("bl", g.session.BuildLabel)
	params.Set("f.sid", g.session.SessionID)

	form := url.Values{}
	form.Set("at", g.session.AccessToken)
	form.Set("f.req", string(fReqBytes))

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, geminiWebGenerateURL+"?"+params.Encode(), strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	g.addCommonHeaders(req)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded;charset=utf-8")
	req.Header.Set("X-Same-Domain", "1")
	req.Header.Set("x-goog-ext-525005358-jspb", fmt.Sprintf(`["%s",1]`, uuidVal))
	req.Header.Set("x-goog-ext-73010989-jspb", "[0]")
	req.Header.Set("x-goog-ext-73010990-jspb", "[0]")

	status, headers, body, err := g.do(req)
	if err != nil {
		return nil, err
	}
	g.mergeSetCookies(headers)
	if status >= 400 {
		return nil, fmt.Errorf("Gemini Web generate returned status %d: %s", status, truncateString(string(body), 512))
	}
	result := geminiWebExtractGenerateResult(body)
	if video && result.VideoURL == "" && result.CID != "" {
		return result, nil
	}
	if result.Text == "" && result.CID != "" && !video {
		if polled, err := g.pollGeneratedText(ctx, result.CID, 90*time.Second); err == nil && polled != nil {
			return polled, nil
		}
	}
	if result.Text == "" && result.ImageURL == "" && result.ImageID == "" && result.VideoURL == "" && result.CID == "" {
		return nil, errors.New("Gemini Web response did not contain text, media, or conversation metadata")
	}
	return result, nil
}

func (g *geminiWebOfficialClient) pollGeneratedText(ctx context.Context, cid string, timeout time.Duration) (*geminiWebGenerateResult, error) {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(geminiWebPollInterval)
	defer ticker.Stop()

	for {
		result, pending, err := g.readChat(ctx, cid)
		if err == nil && result != nil && strings.TrimSpace(result.Text) != "" {
			return result, nil
		}
		if err != nil && !pending {
			return nil, err
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline.C:
			return nil, errors.New("Gemini Web text polling timed out")
		case <-ticker.C:
		}
	}
}

func (g *geminiWebOfficialClient) pollGeneratedImage(ctx context.Context, cid string, timeout time.Duration) (*geminiWebGenerateResult, error) {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(geminiWebPollInterval)
	defer ticker.Stop()

	for {
		result, pending, err := g.readChat(ctx, cid)
		if err == nil && result != nil && (strings.TrimSpace(result.ImageURL) != "" || strings.TrimSpace(result.ImageID) != "") {
			if result.ImageURL == "" && result.ImageID != "" {
				if resolved, resolveErr := g.resolveGeneratedImageURL(ctx, result); resolveErr == nil && resolved != "" {
					result.ImageURL = resolved
				}
			}
			return result, nil
		}
		if err != nil && !pending {
			return nil, err
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline.C:
			return nil, errors.New("Gemini Web image polling timed out")
		case <-ticker.C:
		}
	}
}

func (g *geminiWebOfficialClient) pollGeneratedVideo(ctx context.Context, cid string, timeout time.Duration) (*geminiWebGenerateResult, error) {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(geminiWebPollInterval)
	defer ticker.Stop()

	for {
		result, pending, err := g.readChat(ctx, cid)
		if err == nil && result != nil && strings.TrimSpace(result.VideoURL) != "" {
			return result, nil
		}
		if err != nil && !pending {
			return nil, err
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline.C:
			return nil, errors.New("Gemini Web video polling timed out")
		case <-ticker.C:
		}
	}
}

func (g *geminiWebOfficialClient) resolveGeneratedImageURL(ctx context.Context, result *geminiWebGenerateResult) (string, error) {
	if result == nil || result.CID == "" || result.RID == "" || result.RCID == "" || result.ImageID == "" {
		return "", errors.New("missing Gemini Web image metadata")
	}
	originalURL, err := g.getFullSizeImageURL(ctx, result.CID, result.RID, result.RCID, result.ImageID)
	if err != nil || originalURL == "" {
		return "", err
	}
	first, err := g.fetchGeminiImageTextURL(ctx, originalURL+"=d-I?alr=yes")
	if err != nil || strings.TrimSpace(first) == "" {
		return originalURL, nil
	}
	second, err := g.fetchGeminiImageTextURL(ctx, strings.TrimSpace(first))
	if err != nil || strings.TrimSpace(second) == "" {
		return strings.TrimSpace(first), nil
	}
	return strings.TrimSpace(second), nil
}

func (g *geminiWebOfficialClient) getFullSizeImageURL(ctx context.Context, cid string, rid string, rcid string, imageID string) (string, error) {
	payload := []any{
		[]any{
			[]any{nil, nil, nil, []any{nil, nil, nil, nil, nil, ""}},
			[]any{imageID, 0},
			nil,
			[]any{19, ""},
			nil,
			nil,
			nil,
			nil,
			nil,
			"",
		},
		[]any{rid, rcid, cid, nil, ""},
		1,
		0,
		1,
	}
	payloadBytes, _ := json.Marshal(payload)
	rpc := []any{[]any{[]any{"c8o8Fe", string(payloadBytes), nil, "generic"}}}
	fReqBytes, _ := json.Marshal(rpc)

	params := url.Values{}
	params.Set("rpcids", "c8o8Fe")
	params.Set("hl", g.session.Language)
	params.Set("_reqid", strconv.Itoa(g.nextReqID()))
	params.Set("rt", "c")
	params.Set("source-path", "/app")
	params.Set("bl", g.session.BuildLabel)
	params.Set("f.sid", g.session.SessionID)

	form := url.Values{}
	form.Set("at", g.session.AccessToken)
	form.Set("f.req", string(fReqBytes))

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, geminiWebBatchExecuteURL+"?"+params.Encode(), strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	g.addCommonHeaders(req)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded;charset=utf-8")
	req.Header.Set("X-Same-Domain", "1")
	req.Header.Set("x-goog-ext-525001261-jspb", "[1,null,null,null,null,null,null,null,[4]]")
	req.Header.Set("x-goog-ext-73010989-jspb", "[0]")

	status, headers, body, err := g.do(req)
	if err != nil {
		return "", err
	}
	g.mergeSetCookies(headers)
	if status >= 400 {
		return "", fmt.Errorf("Gemini Web image URL RPC returned status %d", status)
	}
	for _, part := range googleRPCFrames(body) {
		innerStr, _ := getNestedAny(part, []any{2}).(string)
		if innerStr == "" {
			continue
		}
		var payload any
		if err := json.Unmarshal([]byte(innerStr), &payload); err != nil {
			continue
		}
		if url := strings.TrimSpace(stringFromAny(getNestedAny(payload, []any{0}))); url != "" {
			return url, nil
		}
	}
	return "", errors.New("Gemini Web image URL RPC returned no URL")
}

func (g *geminiWebOfficialClient) fetchGeminiImageTextURL(ctx context.Context, rawURL string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Origin", "https://gemini.google.com")
	req.Header.Set("Referer", "https://gemini.google.com/")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/137.0.0.0 Safari/537.36")
	status, _, body, err := g.do(req)
	if err != nil {
		return "", err
	}
	if status >= 400 {
		return "", fmt.Errorf("Gemini Web image URL resolver returned status %d", status)
	}
	text := strings.TrimSpace(string(body))
	if !strings.HasPrefix(text, "http://") && !strings.HasPrefix(text, "https://") {
		return "", errors.New("Gemini Web image URL resolver returned non-URL body")
	}
	return text, nil
}

func (g *geminiWebOfficialClient) fetchGeminiImageDataURL(ctx context.Context, rawURL string) (string, string, error) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" || (!strings.HasPrefix(rawURL, "http://") && !strings.HasPrefix(rawURL, "https://")) {
		return "", "", errors.New("Gemini Web image preview requires an HTTP URL")
	}
	queue := []string{rawURL, geminiWebImageResolverURL(rawURL)}
	seen := make(map[string]struct{}, len(queue))
	var lastErr error
	for len(queue) > 0 && len(seen) < 8 {
		current := strings.TrimSpace(queue[0])
		queue = queue[1:]
		if current == "" {
			continue
		}
		if _, ok := seen[current]; ok {
			continue
		}
		seen[current] = struct{}{}

		dataURL, mimeType, nextURL, err := g.fetchGeminiImageDataURLOnce(ctx, current)
		if err == nil && dataURL != "" {
			return dataURL, mimeType, nil
		}
		if err != nil {
			lastErr = err
		}
		if nextURL != "" {
			queue = append(queue, nextURL, geminiWebImageResolverURL(nextURL))
		}
	}
	if lastErr != nil {
		return "", "", lastErr
	}
	return "", "", errors.New("Gemini Web image preview returned no downloadable image")
}

func (g *geminiWebOfficialClient) fetchGeminiImageDataURLOnce(ctx context.Context, rawURL string) (string, string, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", "", "", err
	}
	g.addCommonHeaders(req)
	req.Header.Set("Accept", "image/avif,image/webp,image/apng,image/svg+xml,image/*,*/*;q=0.8")
	status, headers, body, err := g.do(req)
	if err != nil {
		return "", "", "", err
	}
	if nextURL := geminiWebTextURLFromBytes(headers.Get("Content-Type"), body); nextURL != "" {
		return "", "", nextURL, nil
	}
	if status >= 400 {
		return "", "", "", fmt.Errorf("Gemini Web image preview returned status %d", status)
	}
	dataURL, mimeType, err := geminiWebImageDataURLFromBytes(headers.Get("Content-Type"), body)
	return dataURL, mimeType, "", err
}

func geminiWebImageResolverURL(rawURL string) string {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" || strings.Contains(rawURL, "=d-I?alr=yes") {
		return rawURL
	}
	return rawURL + "=d-I?alr=yes"
}

func geminiWebTextURLFromBytes(contentType string, body []byte) string {
	mimeType := strings.ToLower(strings.TrimSpace(strings.Split(contentType, ";")[0]))
	if mimeType != "text/plain" && mimeType != "text/html" && mimeType != "" {
		return ""
	}
	text := strings.TrimSpace(string(body))
	if strings.HasPrefix(text, "http://") || strings.HasPrefix(text, "https://") {
		return text
	}
	return ""
}

func geminiWebImageDataURLFromBytes(contentType string, body []byte) (string, string, error) {
	if len(body) == 0 {
		return "", "", errors.New("Gemini Web image preview response was empty")
	}
	mimeType := strings.ToLower(strings.TrimSpace(strings.Split(contentType, ";")[0]))
	if mimeType == "" || mimeType == "application/octet-stream" {
		mimeType = strings.ToLower(http.DetectContentType(body))
	}
	if !strings.HasPrefix(mimeType, "image/") {
		return "", "", fmt.Errorf("Gemini Web image preview returned non-image content type %s", mimeType)
	}
	dataURL := "data:" + mimeType + ";base64," + base64.StdEncoding.EncodeToString(body)
	return dataURL, mimeType, nil
}

func (g *geminiWebOfficialClient) readChat(ctx context.Context, cid string) (*geminiWebGenerateResult, bool, error) {
	if g.session == nil {
		if err := g.Init(ctx); err != nil {
			return nil, false, err
		}
	}
	payloadBytes, _ := json.Marshal([]any{cid, 10, nil, 1, []any{1}, []any{4}, nil, 1})
	rpc := []any{[]any{[]any{"hNvQHb", string(payloadBytes), nil, "generic"}}}
	fReqBytes, _ := json.Marshal(rpc)

	params := url.Values{}
	params.Set("rpcids", "hNvQHb")
	params.Set("hl", g.session.Language)
	params.Set("_reqid", strconv.Itoa(g.nextReqID()))
	params.Set("rt", "c")
	params.Set("source-path", "/app")
	params.Set("bl", g.session.BuildLabel)
	params.Set("f.sid", g.session.SessionID)

	form := url.Values{}
	form.Set("at", g.session.AccessToken)
	form.Set("f.req", string(fReqBytes))

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, geminiWebBatchExecuteURL+"?"+params.Encode(), strings.NewReader(form.Encode()))
	if err != nil {
		return nil, false, err
	}
	g.addCommonHeaders(req)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded;charset=utf-8")
	req.Header.Set("X-Same-Domain", "1")
	req.Header.Set("x-goog-ext-525001261-jspb", "[1,null,null,null,null,null,null,null,[4]]")
	req.Header.Set("x-goog-ext-73010989-jspb", "[0]")

	status, headers, body, err := g.do(req)
	if err != nil {
		return nil, true, err
	}
	g.mergeSetCookies(headers)
	if status >= 400 {
		return nil, false, fmt.Errorf("Gemini Web read_chat returned status %d", status)
	}
	return geminiWebExtractReadChatResult(body, cid)
}

func (g *geminiWebOfficialClient) addCommonHeaders(req *http.Request) {
	req.Header.Set("Cookie", g.cookieHeader)
	req.Header.Set("Origin", "https://gemini.google.com")
	req.Header.Set("Referer", "https://gemini.google.com/")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/137.0.0.0 Safari/537.36")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
}

func (g *geminiWebOfficialClient) do(req *http.Request) (int, http.Header, []byte, error) {
	proxyURL := ""
	if g.account != nil && g.account.ProxyID != nil && g.account.Proxy != nil {
		proxyURL = g.account.Proxy.URL()
	}
	resp, err := g.httpUpstream.Do(req, proxyURL, g.account.ID, g.account.Concurrency)
	if err != nil {
		return 0, nil, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, geminiWebMaxResponseBytes))
	if readErr != nil {
		return resp.StatusCode, resp.Header.Clone(), nil, readErr
	}
	return resp.StatusCode, resp.Header.Clone(), body, nil
}

func (g *geminiWebOfficialClient) mergeSetCookies(headers http.Header) {
	if headers == nil {
		return
	}
	changed := false
	for _, raw := range headers.Values("Set-Cookie") {
		name, value := splitCookiePart(strings.SplitN(raw, ";", 2)[0])
		if name == "" || value == "" {
			continue
		}
		g.cookies[name] = value
		changed = true
	}
	if changed {
		g.cookieHeader = geminiWebCookieHeader(g.cookies)
	}
}

func (g *geminiWebOfficialClient) nextReqID() int {
	if g.session == nil {
		return int(time.Now().UnixNano() % 100000)
	}
	if g.session.ReqID <= 0 {
		g.session.ReqID = 10000
	}
	current := g.session.ReqID
	g.session.ReqID += 100000
	return current
}

func extractGoogleBootValue(body, key string) string {
	if body == "" || key == "" {
		return ""
	}
	quoted := regexp.QuoteMeta(key)
	patterns := []string{
		`"` + quoted + `"\s*:\s*"((?:\\.|[^"\\])*)"`,
		`"` + quoted + `"\s*,\s*"((?:\\.|[^"\\])*)"`,
	}
	for _, pattern := range patterns {
		re := regexp.MustCompile(pattern)
		if m := re.FindStringSubmatch(body); len(m) == 2 {
			var out string
			if err := json.Unmarshal([]byte(`"`+m[1]+`"`), &out); err == nil {
				return out
			}
			return m[1]
		}
	}
	return ""
}

func geminiWebExtractGenerateResult(body []byte) *geminiWebGenerateResult {
	result := &geminiWebGenerateResult{}
	for _, part := range googleRPCFrames(body) {
		inner := getNestedAny(part, []any{2})
		innerStr, _ := inner.(string)
		if innerStr == "" {
			continue
		}
		var payload any
		if err := json.Unmarshal([]byte(innerStr), &payload); err != nil {
			continue
		}
		if metadata, ok := getNestedAny(payload, []any{1}).([]any); ok {
			if cid, _ := getNestedAny(metadata, []any{0}).(string); cid != "" {
				result.CID = cid
			}
			if rid, _ := getNestedAny(metadata, []any{1}).(string); rid != "" {
				result.RID = rid
			}
		}
		candidates, _ := getNestedAny(payload, []any{4}).([]any)
		mergeCandidateResult(result, candidates)
	}
	return result
}

func geminiWebExtractReadChatResult(body []byte, fallbackCID string) (*geminiWebGenerateResult, bool, error) {
	var best *geminiWebGenerateResult
	pending := false
	for _, part := range googleRPCFrames(body) {
		innerStr, _ := getNestedAny(part, []any{2}).(string)
		if innerStr == "" {
			continue
		}
		var payload any
		if err := json.Unmarshal([]byte(innerStr), &payload); err != nil {
			continue
		}
		turns, _ := getNestedAny(payload, []any{0}).([]any)
		for _, turn := range turns {
			rid, _ := getNestedAny(turn, []any{0, 1}).(string)
			candidates, _ := getNestedAny(turn, []any{3, 0}).([]any)
			if len(candidates) == 0 {
				continue
			}
			result := &geminiWebGenerateResult{CID: fallbackCID, RID: rid}
			mergeCandidateResult(result, candidates)
			for _, candidate := range candidates {
				if getNestedAny(candidate, []any{12, 6, 0}) != nil {
					pending = true
				}
			}
			if result.VideoURL != "" || result.ImageURL != "" || result.ImageID != "" || result.Text != "" {
				best = result
				if result.VideoURL != "" || result.ImageURL != "" {
					return result, false, nil
				}
			}
		}
	}
	if best != nil {
		return best, pending, nil
	}
	return nil, pending, nil
}

func mergeCandidateResult(result *geminiWebGenerateResult, candidates []any) {
	if result == nil {
		return
	}
	for _, candidate := range candidates {
		if rcid, _ := getNestedAny(candidate, []any{0}).(string); rcid != "" {
			result.RCID = rcid
		}
		if text, _ := getNestedAny(candidate, []any{1, 0}).(string); text != "" {
			if result.ImageID == "" {
				result.ImageID = firstGeminiWebImageArtifact(text)
			}
			cleaned := strings.TrimSpace(geminiWebArtifactPattern.ReplaceAllString(text, ""))
			if cleaned != "" {
				result.Text = cleaned
			}
		}
		if url, imageID := extractGeminiWebImage(candidate); url != "" || imageID != "" {
			if result.ImageURL == "" {
				result.ImageURL = url
			}
			if result.ImageID == "" {
				result.ImageID = imageID
			}
		}
		if url, thumb := extractGeminiWebVideoURL(candidate); url != "" {
			result.VideoURL = url
			result.ThumbnailURL = thumb
			return
		}
	}
}

func firstGeminiWebImageArtifact(text string) string {
	for _, match := range geminiWebArtifactPattern.FindAllString(text, -1) {
		match = strings.TrimSpace(match)
		if strings.Contains(match, "/image_generation_content/") {
			return match
		}
	}
	return ""
}

func extractGeminiWebImage(candidate any) (string, string) {
	if webImages, ok := getNestedAny(candidate, []any{12, 1}).([]any); ok {
		for _, image := range webImages {
			if url := strings.TrimSpace(stringFromAny(getNestedAny(image, []any{0, 0, 0}))); url != "" {
				return url, ""
			}
		}
	}

	imageGroups := [][]any{
		{12, 7, 0},
		{12, 0, "8", 0},
	}
	for _, path := range imageGroups {
		images, ok := getNestedAny(candidate, path).([]any)
		if !ok {
			continue
		}
		for idx, image := range images {
			url := strings.TrimSpace(stringFromAny(getNestedAny(image, []any{0, 3, 3})))
			imageID := strings.TrimSpace(stringFromAny(getNestedAny(image, []any{1, 0})))
			if imageID == "" {
				imageID = fmt.Sprintf("http://googleusercontent.com/image_generation_content/%d", idx)
			}
			if url != "" || imageID != "" {
				return url, imageID
			}
		}
	}
	return "", ""
}

func extractGeminiWebVideoURL(candidate any) (string, string) {
	paths := [][]any{
		{12, 59, 0, 0, 0, 0, 7},
		{12, 8, 60, 0, 0, 0, 0, 7},
		{12, 8, "60", 0, 0, 0, 0, 7},
		{12, 0, 60, 0, 0, 0, 0, 7},
		{12, 0, "60", 0, 0, 0, 0, 7},
	}
	for _, path := range paths {
		urls, _ := getNestedAny(candidate, path).([]any)
		if len(urls) < 2 {
			continue
		}
		thumb, _ := urls[0].(string)
		video, _ := urls[1].(string)
		if strings.TrimSpace(video) != "" {
			return video, thumb
		}
	}
	protobufURL, _ := getNestedAny(candidate, []any{12, 0, "60", 0, 0, 8, 7, 1}).(string)
	thumb, _ := getNestedAny(candidate, []any{12, 0, "60", 0, 0, 0, 0, 7, 0}).(string)
	return strings.TrimSpace(protobufURL), thumb
}

func googleRPCFrames(body []byte) []any {
	content := string(body)
	if strings.HasPrefix(content, ")]}'") {
		content = strings.TrimLeft(content[4:], "\r\n\t ")
	}
	var frames []any
	lines := strings.Split(content, "\n")
	for i := 0; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		if geminiWebFrameLengthLine.MatchString(line) {
			for i+1 < len(lines) && strings.TrimSpace(lines[i+1]) == "" {
				i++
			}
			if i+1 < len(lines) {
				i++
				appendParsedGoogleFrame(&frames, strings.TrimSpace(lines[i]))
			}
			continue
		}
		appendParsedGoogleFrame(&frames, line)
	}
	if len(frames) > 0 {
		return frames
	}
	var whole any
	if err := json.Unmarshal([]byte(strings.TrimSpace(content)), &whole); err == nil {
		appendGoogleFrameValue(&frames, whole)
	}
	return frames
}

func appendParsedGoogleFrame(frames *[]any, raw string) {
	if raw == "" || (!strings.HasPrefix(raw, "[") && !strings.HasPrefix(raw, "{")) {
		return
	}
	var parsed any
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return
	}
	appendGoogleFrameValue(frames, parsed)
}

func appendGoogleFrameValue(frames *[]any, value any) {
	switch v := value.(type) {
	case []any:
		if len(v) >= 3 {
			if _, ok := v[0].(string); ok {
				*frames = append(*frames, v)
				return
			}
		}
		*frames = append(*frames, v...)
	case map[string]any:
		*frames = append(*frames, v)
	}
}

func getNestedAny(data any, path []any) any {
	current := data
	for _, key := range path {
		switch k := key.(type) {
		case int:
			arr, ok := current.([]any)
			if !ok || k < 0 || k >= len(arr) {
				return nil
			}
			current = arr[k]
		case string:
			obj, ok := current.(map[string]any)
			if !ok {
				return nil
			}
			v, ok := obj[k]
			if !ok {
				return nil
			}
			current = v
		default:
			return nil
		}
	}
	return current
}

func estimateTextTokens(text string) int {
	text = strings.TrimSpace(text)
	if text == "" {
		return 0
	}
	runes := len([]rune(text))
	if runes < 4 {
		return 1
	}
	return runes / 4
}

func geminiWebResultContentJSON(result *geminiWebGenerateResult, kind string) string {
	if result == nil {
		return ""
	}
	payload := map[string]any{
		"type": kind,
	}
	if result.Text != "" {
		payload["text"] = result.Text
	}
	if result.ImageURL != "" {
		payload["url"] = result.ImageURL
		payload["image_url"] = result.ImageURL
	}
	if result.ImageID != "" {
		payload["image_id"] = result.ImageID
	}
	if result.VideoURL != "" {
		payload["url"] = result.VideoURL
	}
	if result.ThumbnailURL != "" {
		payload["thumbnail_url"] = result.ThumbnailURL
	}
	if result.CID != "" {
		payload["cid"] = result.CID
	}
	if result.RID != "" {
		payload["rid"] = result.RID
	}
	if result.RCID != "" {
		payload["rcid"] = result.RCID
	}
	b, _ := json.Marshal(payload)
	return string(bytes.TrimSpace(b))
}
