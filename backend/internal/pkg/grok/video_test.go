package grok

import "testing"

func TestVideoPublicAndDownloadURLs(t *testing.T) {
	postID := "b63771fb-a88d-4f31-8f81-3703c54a87c9"

	if got, want := publicVideoURL(postID), videoPublicBaseURL+"/"+postID+".mp4?dl=0"; got != want {
		t.Fatalf("publicVideoURL() = %q, want %q", got, want)
	}
	if got, want := downloadVideoURL(postID), videoPublicBaseURL+"/"+postID+".mp4?dl=1"; got != want {
		t.Fatalf("downloadVideoURL() = %q, want %q", got, want)
	}
}

func TestPublicVideoURLFromShareLink(t *testing.T) {
	postID := "b63771fb-a88d-4f31-8f81-3703c54a87c9"

	tests := []struct {
		name      string
		shareLink string
		want      string
	}{
		{
			name:      "share page falls back to public asset",
			shareLink: "https://grok.com/imagine/post/b63771fb-a88d-4f31-8f81-3703c54a87c9?source=post-page&platform=web",
			want:      videoPublicBaseURL + "/" + postID + ".mp4?dl=0",
		},
		{
			name:      "public asset normalizes playback query",
			shareLink: videoPublicBaseURL + "/" + postID + ".mp4?cache=1",
			want:      videoPublicBaseURL + "/" + postID + ".mp4?dl=0",
		},
		{
			name:      "direct mp4 from another host is preserved",
			shareLink: "https://cdn.example.com/video.mp4?sig=1",
			want:      "https://cdn.example.com/video.mp4?sig=1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := publicVideoURLFromShareLink(postID, tt.shareLink); got != tt.want {
				t.Fatalf("publicVideoURLFromShareLink() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDownloadVideoURLFromPublicURL(t *testing.T) {
	postID := "b63771fb-a88d-4f31-8f81-3703c54a87c9"

	if got, want := downloadVideoURLFromPublicURL(postID, videoPublicBaseURL+"/"+postID+".mp4?dl=0"), videoPublicBaseURL+"/"+postID+".mp4?dl=1"; got != want {
		t.Fatalf("downloadVideoURLFromPublicURL() = %q, want %q", got, want)
	}
	if got, want := downloadVideoURLFromPublicURL(postID, "https://cdn.example.com/video.mp4?sig=1"), videoPublicBaseURL+"/"+postID+".mp4?dl=1"; got != want {
		t.Fatalf("downloadVideoURLFromPublicURL() = %q, want %q", got, want)
	}
}
