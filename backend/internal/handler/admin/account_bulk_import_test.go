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
	}, 1)

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
	}, 1)

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

func TestValidateBulkImportedAccountWrapsRunnerError(t *testing.T) {
	handler := &AccountHandler{
		accountTestService: &bulkImportTestRunnerStub{err: errors.New("network down")},
	}

	err := handler.validateBulkImportedAccount(context.Background(), &service.Account{ID: 42})
	if err == nil || err.Error() != "account validation failed: network down" {
		t.Fatalf("unexpected error: %v", err)
	}
}
