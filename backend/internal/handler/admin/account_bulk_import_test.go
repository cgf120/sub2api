package admin

import "testing"

func TestNormalizeAccountBulkImportType(t *testing.T) {
	tests := map[string]string{
		"gpt":      "gpt",
		" OpenAI ": "gpt",
		"chatgpt":  "gpt",
		"grok":     "grok",
		"x.ai":     "grok",
		"other":    "",
	}

	for input, want := range tests {
		if got := normalizeAccountBulkImportType(input); got != want {
			t.Fatalf("normalizeAccountBulkImportType(%q)=%q, want %q", input, got, want)
		}
	}
}

func TestNormalizeAccountBulkImportItems(t *testing.T) {
	items := normalizeAccountBulkImportItems([]AccountBulkImportItem{
		{Name: " ", Type: " ", Credential: " "},
		{Name: " acc ", Type: " gpt ", Credential: " sk-test "},
		{RowNumber: 9, Type: "grok", Credential: "sso"},
	})

	if len(items) != 2 {
		t.Fatalf("len(items)=%d, want 2", len(items))
	}
	if items[0].RowNumber != 2 || items[0].Name != "acc" || items[0].Type != "gpt" || items[0].Credential != "sk-test" {
		t.Fatalf("unexpected first item: %+v", items[0])
	}
	if items[1].RowNumber != 9 {
		t.Fatalf("row number=%d, want 9", items[1].RowNumber)
	}
}

func TestBuildAccountBulkImportCreateConfig(t *testing.T) {
	platform, accountType, credentials, prefix, err := buildAccountBulkImportCreateConfig(AccountBulkImportItem{
		Type:       "gpt",
		Credential: "sk-test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if platform != "openai" || accountType != "apikey" || prefix != "GPT" || credentials["api_key"] != "sk-test" {
		t.Fatalf("unexpected gpt config: platform=%s type=%s prefix=%s credentials=%v", platform, accountType, prefix, credentials)
	}

	platform, accountType, credentials, prefix, err = buildAccountBulkImportCreateConfig(AccountBulkImportItem{
		Type:       "grok",
		Credential: "sso-test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if platform != "grok" || accountType != "oauth" || prefix != "Grok" || credentials["sso"] != "sso-test" {
		t.Fatalf("unexpected grok config: platform=%s type=%s prefix=%s credentials=%v", platform, accountType, prefix, credentials)
	}
}
