package admin

import (
	"context"
	"errors"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

type bulkImportTestRunnerStub struct {
	result    *service.ScheduledTestResult
	err       error
	accountID int64
	calls     int
}

func (s *bulkImportTestRunnerStub) TestAccountConnection(_ *gin.Context, _ int64, _ string, _ string, _ string) error {
	return nil
}

func (s *bulkImportTestRunnerStub) RunTestBackground(_ context.Context, accountID int64, _ string) (*service.ScheduledTestResult, error) {
	s.calls++
	s.accountID = accountID
	return s.result, s.err
}

func (s *bulkImportTestRunnerStub) ProbeOpenAIAPIKeyResponsesSupport(_ context.Context, _ int64) {}

func (s *bulkImportTestRunnerStub) FetchUpstreamSupportedModels(_ context.Context, _ *service.Account) ([]string, error) {
	return nil, nil
}

func TestNormalizeAccountBulkImportType(t *testing.T) {
	tests := map[string]string{
		"gpt":        "gpt",
		" OpenAI ":   "gpt",
		"chatgpt":    "gpt",
		"grok":       "grok",
		"x.ai":       "grok",
		"gemini":     "gemini",
		"Google":     "gemini",
		"ai_studio":  "gemini",
		"anthropic":  "anthropic",
		"Claude":     "anthropic",
		"claude_api": "anthropic",
		"other":      "",
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

	platform, accountType, credentials, prefix, err = buildAccountBulkImportCreateConfig(AccountBulkImportItem{
		Type:       "gemini",
		Credential: "gemini-key-test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if platform != "gemini" ||
		accountType != "apikey" ||
		prefix != "Gemini" ||
		credentials["api_key"] != "gemini-key-test" ||
		credentials["base_url"] != "https://generativelanguage.googleapis.com" ||
		credentials["tier_id"] != service.GeminiTierAIStudioFree {
		t.Fatalf("unexpected gemini config: platform=%s type=%s prefix=%s credentials=%v", platform, accountType, prefix, credentials)
	}

	platform, accountType, credentials, prefix, err = buildAccountBulkImportCreateConfig(AccountBulkImportItem{
		Type:       "anthropic",
		Credential: "sk-ant-test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if platform != "anthropic" ||
		accountType != "apikey" ||
		prefix != "Anthropic" ||
		credentials["api_key"] != "sk-ant-test" ||
		credentials["base_url"] != "https://api.anthropic.com" {
		t.Fatalf("unexpected anthropic config: platform=%s type=%s prefix=%s credentials=%v", platform, accountType, prefix, credentials)
	}
}

func TestCreateAccountFromBulkImportItemValidatesBeforeSuccess(t *testing.T) {
	adminSvc := newStubAdminService()
	testRunner := &bulkImportTestRunnerStub{
		result: &service.ScheduledTestResult{Status: "success"},
	}
	handler := &AccountHandler{
		adminService:       adminSvc,
		accountTestService: testRunner,
	}

	result := handler.createAccountFromBulkImportItem(context.Background(), AccountBulkImportItem{
		RowNumber:  2,
		Name:       "grok-valid",
		Type:       "grok",
		Credential: "sso-valid",
	}, 1, accountBulkImportOptions{})

	if !result.Success || result.AccountID != 300 {
		t.Fatalf("expected successful import with account id, got %+v", result)
	}
	if testRunner.calls != 1 || testRunner.accountID != 300 {
		t.Fatalf("expected validation to run once for account 300, calls=%d accountID=%d", testRunner.calls, testRunner.accountID)
	}
	if len(adminSvc.deletedAccountIDs) != 0 {
		t.Fatalf("expected no rollback delete, got %v", adminSvc.deletedAccountIDs)
	}
}

func TestCreateAccountFromBulkImportItemDisablesFailedValidation(t *testing.T) {
	adminSvc := newStubAdminService()
	testRunner := &bulkImportTestRunnerStub{
		result: &service.ScheduledTestResult{Status: "failed", ErrorMessage: "invalid sso"},
	}
	handler := &AccountHandler{
		adminService:       adminSvc,
		accountTestService: testRunner,
	}

	result := handler.createAccountFromBulkImportItem(context.Background(), AccountBulkImportItem{
		RowNumber:  3,
		Type:       "grok",
		Credential: "sso-invalid",
	}, 1, accountBulkImportOptions{})

	if !result.Success || result.AccountID != 300 {
		t.Fatalf("expected imported disabled account, got %+v", result)
	}
	if result.Warning != "account validation failed: invalid sso; account imported as disabled" {
		t.Fatalf("unexpected warning: %q", result.Warning)
	}
	if len(adminSvc.deletedAccountIDs) != 0 {
		t.Fatalf("expected no rollback delete, got %v", adminSvc.deletedAccountIDs)
	}
	if adminSvc.updatedAccountStatus[300] != service.StatusDisabled {
		t.Fatalf("expected account 300 disabled, got %q", adminSvc.updatedAccountStatus[300])
	}
	if adminSvc.accountSchedulable[300] {
		t.Fatalf("expected account 300 unschedulable")
	}
	if adminSvc.accountErrors[300] != "account validation failed: invalid sso" {
		t.Fatalf("expected validation error recorded, got %q", adminSvc.accountErrors[300])
	}
}

func TestCreateAccountFromBulkImportItemKeepsGeminiActiveForTransientValidation(t *testing.T) {
	adminSvc := newStubAdminService()
	testRunner := &bulkImportTestRunnerStub{
		result: &service.ScheduledTestResult{
			Status:       "failed",
			ErrorMessage: `API returned 503: {"error":{"status":"UNAVAILABLE","message":"This model is currently experiencing high demand. Please try again later."}}`,
		},
	}
	handler := &AccountHandler{
		adminService:       adminSvc,
		accountTestService: testRunner,
	}

	result := handler.createAccountFromBulkImportItem(context.Background(), AccountBulkImportItem{
		RowNumber:  4,
		Type:       "gemini",
		Credential: "gemini-key-test",
	}, 1, accountBulkImportOptions{})

	if !result.Success || result.AccountID != 300 {
		t.Fatalf("expected inconclusive Gemini validation to keep imported account active, got %+v", result)
	}
	if result.Warning != `account validation failed: API returned 503: {"error":{"status":"UNAVAILABLE","message":"This model is currently experiencing high demand. Please try again later."}}; validation inconclusive, account imported as active` {
		t.Fatalf("unexpected warning: %q", result.Warning)
	}
	if len(adminSvc.deletedAccountIDs) != 0 {
		t.Fatalf("expected no rollback delete, got %v", adminSvc.deletedAccountIDs)
	}
	if _, ok := adminSvc.updatedAccountStatus[300]; ok {
		t.Fatalf("expected account 300 status not to be updated, got %q", adminSvc.updatedAccountStatus[300])
	}
	if _, ok := adminSvc.accountSchedulable[300]; ok {
		t.Fatalf("expected account 300 schedulable flag not to be changed")
	}
	if _, ok := adminSvc.accountErrors[300]; ok {
		t.Fatalf("expected account 300 error not to be recorded, got %q", adminSvc.accountErrors[300])
	}
}

func TestCreateAccountFromBulkImportItemAppliesProxyAndPlatformGroups(t *testing.T) {
	adminSvc := newStubAdminService()
	testRunner := &bulkImportTestRunnerStub{
		result: &service.ScheduledTestResult{Status: "success"},
	}
	proxyID := int64(12)
	handler := &AccountHandler{
		adminService:       adminSvc,
		accountTestService: testRunner,
	}

	result := handler.createAccountFromBulkImportItem(context.Background(), AccountBulkImportItem{
		RowNumber:  5,
		Type:       "gemini",
		Credential: "gemini-key-test",
	}, 1, accountBulkImportOptions{
		GroupIDs: []int64{10, 20, 30},
		ProxyID:  &proxyID,
		groupPlatformByID: map[int64]string{
			10: service.PlatformOpenAI,
			20: service.PlatformGemini,
			30: service.PlatformGemini,
		},
	})

	if !result.Success {
		t.Fatalf("expected successful import, got %+v", result)
	}
	if len(adminSvc.createdAccounts) != 1 {
		t.Fatalf("created accounts=%d, want 1", len(adminSvc.createdAccounts))
	}
	input := adminSvc.createdAccounts[0]
	if input.ProxyID == nil || *input.ProxyID != proxyID {
		t.Fatalf("proxy id=%v, want %d", input.ProxyID, proxyID)
	}
	if len(input.GroupIDs) != 2 || input.GroupIDs[0] != 20 || input.GroupIDs[1] != 30 {
		t.Fatalf("group ids=%v, want [20 30]", input.GroupIDs)
	}
}

func TestAccountBulkImportOptionsGroupIDsForPlatformFallsBackToDefaultGroup(t *testing.T) {
	options := accountBulkImportOptions{
		GroupIDs: []int64{10},
		groupPlatformByID: map[int64]string{
			10: service.PlatformOpenAI,
		},
	}

	got := options.groupIDsForPlatform(service.PlatformGemini)
	if got != nil {
		t.Fatalf("group ids=%v, want nil", got)
	}
}

func TestBuildAccountBulkImportOptionsNormalizesAndValidatesGroups(t *testing.T) {
	adminSvc := newStubAdminService()
	adminSvc.groups = []service.Group{
		{ID: 10, Platform: service.PlatformOpenAI, Status: service.StatusActive},
		{ID: 20, Platform: service.PlatformGemini, Status: service.StatusActive},
	}
	proxyID := int64(99)
	handler := &AccountHandler{adminService: adminSvc}

	options, err := handler.buildAccountBulkImportOptions(context.Background(), AccountBulkImportRequest{
		GroupIDs: []int64{0, 10, 10, 20, -1},
		ProxyID:  &proxyID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(options.GroupIDs) != 2 || options.GroupIDs[0] != 10 || options.GroupIDs[1] != 20 {
		t.Fatalf("group ids=%v, want [10 20]", options.GroupIDs)
	}
	if options.ProxyID == nil || *options.ProxyID != proxyID {
		t.Fatalf("proxy id=%v, want %d", options.ProxyID, proxyID)
	}
	if options.groupPlatformByID[10] != service.PlatformOpenAI || options.groupPlatformByID[20] != service.PlatformGemini {
		t.Fatalf("unexpected group platform map: %v", options.groupPlatformByID)
	}

	_, err = handler.buildAccountBulkImportOptions(context.Background(), AccountBulkImportRequest{
		GroupIDs: []int64{30},
	})
	if err == nil || err.Error() != "group 30 not found" {
		t.Fatalf("unexpected missing group error: %v", err)
	}
}

func TestValidateBulkImportedAccountWrapsRunnerError(t *testing.T) {
	handler := &AccountHandler{
		accountTestService: &bulkImportTestRunnerStub{err: errors.New("network down")},
	}

	err := handler.validateBulkImportedAccount(context.Background(), &service.Account{ID: 42})
	if err == nil || err.Error() != "account validation failed: network down" {
		t.Fatalf("unexpected error: %v", err)
	}
}
