package geminicli

import "testing"

func TestDefaultModels_ContainsImageModels(t *testing.T) {
	t.Parallel()

	byID := make(map[string]Model, len(DefaultModels))
	for _, model := range DefaultModels {
		byID[model.ID] = model
	}

	required := []string{
		"gemini-2.5-flash-image",
		"gemini-3.1-flash-image",
	}

	for _, id := range required {
		if _, ok := byID[id]; !ok {
			t.Fatalf("expected curated Gemini model %q to exist", id)
		}
	}
}

func TestDefaultTestModelUsesTextModel(t *testing.T) {
	if DefaultTestModel != "gemini-3-flash-preview" {
		t.Fatalf("DefaultTestModel=%q, want gemini-3-flash-preview", DefaultTestModel)
	}
}
