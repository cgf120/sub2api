package grok

import "strings"

const (
	ModelChatFast     = "grok-4.20-fast"
	ModelChatAuto     = "grok-4.20-auto"
	ModelChatExpert   = "grok-4.20-expert"
	ModelImageLite    = "grok-imagine-image-lite"
	ModelImage        = "grok-imagine-image"
	ModelImagePro     = "grok-imagine-image-pro"
	ModelImagineVideo = "grok-imagine-video"
)

type Model struct {
	ID          string `json:"id"`
	Object      string `json:"object"`
	Created     int64  `json:"created"`
	OwnedBy     string `json:"owned_by"`
	Type        string `json:"type,omitempty"`
	DisplayName string `json:"display_name,omitempty"`
}

var DefaultModels = []Model{
	{ID: ModelChatFast, Object: "model", Created: 1704067200, OwnedBy: "xai", Type: "model", DisplayName: "Grok 4.20 Fast"},
	{ID: ModelChatAuto, Object: "model", Created: 1704067200, OwnedBy: "xai", Type: "model", DisplayName: "Grok 4.20 Auto"},
	{ID: ModelChatExpert, Object: "model", Created: 1704067200, OwnedBy: "xai", Type: "model", DisplayName: "Grok 4.20 Expert"},
	{ID: ModelImageLite, Object: "model", Created: 1704067200, OwnedBy: "xai", Type: "model", DisplayName: "Grok Imagine Image Lite"},
	{ID: ModelImage, Object: "model", Created: 1704067200, OwnedBy: "xai", Type: "model", DisplayName: "Grok Imagine Image"},
	{ID: ModelImagePro, Object: "model", Created: 1704067200, OwnedBy: "xai", Type: "model", DisplayName: "Grok Imagine Image Pro"},
	{ID: ModelImagineVideo, Object: "model", Created: 1704067200, OwnedBy: "xai", Type: "model", DisplayName: "Grok Imagine Video"},
}

func DefaultModelIDs() []string {
	ids := make([]string, 0, len(DefaultModels))
	for _, model := range DefaultModels {
		ids = append(ids, model.ID)
	}
	return ids
}

func ModeIDForModel(model string) string {
	switch model {
	case ModelChatFast, ModelImageLite:
		return "fast"
	case ModelChatExpert:
		return "expert"
	default:
		return "auto"
	}
}

func IsImageModel(model string) bool {
	switch strings.TrimSpace(model) {
	case ModelImageLite, ModelImage, ModelImagePro:
		return true
	default:
		return false
	}
}

func IsVideoModel(model string) bool {
	return strings.TrimSpace(model) == ModelImagineVideo
}

func IsMediaModel(model string) bool {
	return IsImageModel(model) || IsVideoModel(model)
}
