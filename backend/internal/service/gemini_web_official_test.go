package service

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
)

type geminiWebHTTPUpstreamStub struct {
	do func(req *http.Request) (*http.Response, error)
}

func (s *geminiWebHTTPUpstreamStub) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	if s.do != nil {
		return s.do(req)
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader("")),
	}, nil
}

func (s *geminiWebHTTPUpstreamStub) DoWithTLS(req *http.Request, proxyURL string, accountID int64, accountConcurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return s.Do(req, proxyURL, accountID, accountConcurrency)
}

func TestGeminiWebCookiesFromCredentials(t *testing.T) {
	cookies := geminiWebCookiesFromCredentials(map[string]any{
		"cookies": map[string]any{
			"__Secure-1PSID":   "__Secure-1PSID=psid-value",
			"NID":              "NID=nid-value",
			"COMPASS":          "compass-value",
			"__Secure-1PSIDCC": "cc-value",
		},
		"__Secure-3PSID": "third-psid",
	})

	require.Equal(t, "psid-value", cookies["__Secure-1PSID"])
	require.Equal(t, "nid-value", cookies["NID"])
	require.Equal(t, "compass-value", cookies["COMPASS"])
	require.Equal(t, "cc-value", cookies["__Secure-1PSIDCC"])
	require.Equal(t, "third-psid", cookies["__Secure-3PSID"])
}

func TestGeminiWebCookiesFromWrappedJSONCredentials(t *testing.T) {
	cookies := geminiWebCookiesFromCredentials(map[string]any{
		"cookies": `{"cookies":{"__Secure-1PSID":"psid-value","NID":"nid-value","COMPASS":"compass-value"},"email":"ignored@example.test"}`,
	})

	require.Equal(t, "psid-value", cookies["__Secure-1PSID"])
	require.Equal(t, "nid-value", cookies["NID"])
	require.Equal(t, "compass-value", cookies["COMPASS"])
	require.Empty(t, cookies["email"])
}

func TestNewGeminiWebOfficialClientRejectsGoogleToken(t *testing.T) {
	_, err := newGeminiWebOfficialClient(&Account{
		Platform: PlatformGemini,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"oauth_type":     "web",
			"google_token":   "psid-value::nid-value",
			"__Secure-1PSID": "psid-value",
			"NID":            "nid-value",
		},
	}, &geminiWebHTTPUpstreamStub{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "PSID::NID")
}

func TestNewGeminiWebOfficialClientRejectsRawCookieHeader(t *testing.T) {
	_, err := newGeminiWebOfficialClient(&Account{
		Platform: PlatformGemini,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"oauth_type": "web",
			"cookies":    "__Secure-1PSID=psid-value; NID=nid-value",
		},
	}, &geminiWebHTTPUpstreamStub{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "raw Cookie headers")
}

func TestNewGeminiWebOfficialClientAcceptsMinimalJSONCookies(t *testing.T) {
	client, err := newGeminiWebOfficialClient(&Account{
		Platform: PlatformGemini,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"oauth_type": "web",
			"cookies": map[string]any{
				"__Secure-1PSID": "psid-value",
				"NID":            "nid-value",
			},
		},
	}, &geminiWebHTTPUpstreamStub{})
	require.NoError(t, err)
	require.Equal(t, "psid-value", client.cookies["__Secure-1PSID"])
	require.Equal(t, "nid-value", client.cookies["NID"])
}

func TestGeminiWebAccountCanGenerateVideo(t *testing.T) {
	tests := []struct {
		name string
		tier string
		want bool
	}{
		{name: "missing defaults to free", tier: "", want: false},
		{name: "free", tier: GeminiTierGoogleOneFree, want: false},
		{name: "unknown", tier: GeminiTierGoogleOneUnknown, want: false},
		{name: "legacy free", tier: "FREE", want: false},
		{name: "pro", tier: GeminiTierGoogleAIPro, want: true},
		{name: "ultra", tier: GeminiTierGoogleAIUltra, want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			account := &Account{
				Platform: PlatformGemini,
				Type:     AccountTypeOAuth,
				Credentials: map[string]any{
					"oauth_type": "web",
					"tier_id":    tt.tier,
				},
			}

			require.Equal(t, tt.want, geminiWebAccountCanGenerateVideo(account))
		})
	}
}

func TestGeminiWebGenerateVideoRejectsFreeTierBeforeUpstream(t *testing.T) {
	upstreamCalled := false
	client, err := newGeminiWebOfficialClient(&Account{
		Platform: PlatformGemini,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"oauth_type": "web",
			"tier_id":    GeminiTierGoogleOneFree,
			"cookies": map[string]any{
				"__Secure-1PSID": "psid-value",
				"NID":            "nid-value",
			},
		},
	}, &geminiWebHTTPUpstreamStub{
		do: func(req *http.Request) (*http.Response, error) {
			upstreamCalled = true
			return nil, errors.New("unexpected upstream call")
		},
	})
	require.NoError(t, err)

	result, err := client.GenerateVideo(t.Context(), "make a short video")

	require.Nil(t, result)
	require.Error(t, err)
	require.Contains(t, err.Error(), GeminiTierGoogleOneFree)
	require.False(t, upstreamCalled)
}

func TestNewGeminiWebOfficialClientRequiresPSIDAndNID(t *testing.T) {
	_, err := newGeminiWebOfficialClient(&Account{
		Platform: PlatformGemini,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"oauth_type": "web",
			"cookies": map[string]any{
				"__Secure-1PSID": "psid-only",
			},
		},
	}, &geminiWebHTTPUpstreamStub{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "__Secure-1PSID")
	require.Contains(t, err.Error(), "NID")
}

func TestExtractGeminiWebVideoURL(t *testing.T) {
	urls := []any{"https://example.test/thumb.jpg", "https://example.test/video.mp4"}
	holder := make([]any, 8)
	holder[7] = urls
	candidate := make([]any, 13)
	candidate[12] = []any{
		map[string]any{
			"60": []any{
				[]any{
					[]any{
						[]any{
							holder,
						},
					},
				},
			},
		},
	}

	video, thumb := extractGeminiWebVideoURL(candidate)
	require.Equal(t, "https://example.test/video.mp4", video)
	require.Equal(t, "https://example.test/thumb.jpg", thumb)
}

func TestMergeCandidateResultExtractsGeneratedImage(t *testing.T) {
	genImage := make([]any, 2)
	genImage[0] = []any{nil, nil, nil, []any{nil, nil, "alt text", "https://example.test/generated.png"}}
	genImage[1] = []any{"http://googleusercontent.com/image_generation_content/7"}
	candidate := make([]any, 13)
	candidate[0] = "rcid-1"
	candidate[1] = []any{"http://googleusercontent.com/image_generation_content/7\n"}
	candidate[12] = []any{
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		[]any{[]any{genImage}},
	}

	result := &geminiWebGenerateResult{}
	mergeCandidateResult(result, []any{candidate})

	require.Equal(t, "rcid-1", result.RCID)
	require.Empty(t, result.Text)
	require.Equal(t, "https://example.test/generated.png", result.ImageURL)
	require.Equal(t, "http://googleusercontent.com/image_generation_content/7", result.ImageID)
}

func TestExtractGeminiWebImageFallsBackToImageID(t *testing.T) {
	genImage := make([]any, 2)
	genImage[0] = []any{nil, nil, nil, []any{nil, nil, "alt text", ""}}
	candidate := make([]any, 13)
	candidate[12] = []any{
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		[]any{[]any{genImage}},
	}

	url, imageID := extractGeminiWebImage(candidate)

	require.Empty(t, url)
	require.Equal(t, "http://googleusercontent.com/image_generation_content/0", imageID)
}

func TestGeminiWebImageDataURLFromBytes(t *testing.T) {
	dataURL, mimeType, err := geminiWebImageDataURLFromBytes("image/png; charset=binary", []byte("png-bytes"))

	require.NoError(t, err)
	require.Equal(t, "image/png", mimeType)
	require.Equal(t, "data:image/png;base64,cG5nLWJ5dGVz", dataURL)
}

func TestGeminiWebImageDataURLRejectsNonImage(t *testing.T) {
	dataURL, mimeType, err := geminiWebImageDataURLFromBytes("text/html", []byte("<html></html>"))

	require.Error(t, err)
	require.Empty(t, dataURL)
	require.Empty(t, mimeType)
}

func TestGeminiWebFetchImageDataURLFollowsTextURLChain(t *testing.T) {
	var ts *httptest.Server
	ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.RequestURI() {
		case "/raw":
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte("<html></html>"))
		case "/raw=d-I?alr=yes":
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte(ts.URL + "/text"))
		case "/text":
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte(ts.URL + "/image"))
		case "/image":
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write([]byte("png-bytes"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer ts.Close()

	client := &geminiWebOfficialClient{
		account: &Account{},
		httpUpstream: &geminiWebHTTPUpstreamStub{
			do: func(req *http.Request) (*http.Response, error) {
				return http.DefaultClient.Do(req)
			},
		},
	}

	dataURL, mimeType, err := client.fetchGeminiImageDataURL(t.Context(), ts.URL+"/raw")

	require.NoError(t, err)
	require.Equal(t, "image/png", mimeType)
	require.Equal(t, "data:image/png;base64,cG5nLWJ5dGVz", dataURL)
}

func TestGeminiWebResultContentJSONUsesImageDataURL(t *testing.T) {
	content := geminiWebResultContentJSON(&geminiWebGenerateResult{
		ImageURL: "data:image/png;base64,cG5nLWJ5dGVz",
		ImageID:  "http://googleusercontent.com/image_generation_content/0",
		CID:      "c_test",
		RID:      "r_test",
		RCID:     "rc_test",
	}, "image")

	require.Contains(t, content, `"type":"image"`)
	require.Contains(t, content, `"url":"data:image/png;base64,cG5nLWJ5dGVz"`)
	require.Contains(t, content, `"image_url":"data:image/png;base64,cG5nLWJ5dGVz"`)
	require.Contains(t, content, `"image_id":"http://googleusercontent.com/image_generation_content/0"`)
}

func TestGeminiWebPromptFromChatCompletions(t *testing.T) {
	req := &apicompat.ChatCompletionsRequest{
		Model:        "gemini-3-pro",
		Instructions: "You are concise.",
		Messages: []apicompat.ChatMessage{
			{Role: "user", Content: []byte(`"hello"`)},
			{Role: "assistant", Content: []byte(`"hi"`)},
			{Role: "user", Content: []byte(`[{"type":"text","text":"make a short video prompt"}]`)},
		},
	}

	prompt, err := geminiWebPromptFromChatCompletions(req)
	require.NoError(t, err)
	require.Contains(t, prompt, "You are concise.")
	require.Contains(t, prompt, "user: hello")
	require.Contains(t, prompt, "assistant: hi")
	require.Contains(t, prompt, "user: make a short video prompt")
}

func TestGoogleRPCFramesKeepsSingleFrameArray(t *testing.T) {
	frames := googleRPCFrames([]byte(`["wrb.fr",null,"[]"]`))
	require.Len(t, frames, 1)
	frame, ok := frames[0].([]any)
	require.True(t, ok)
	require.Equal(t, "wrb.fr", frame[0])
}
