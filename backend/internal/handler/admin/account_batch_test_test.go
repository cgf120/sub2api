package admin

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

func TestNormalizeAccountBatchTestIDs(t *testing.T) {
	got := normalizeAccountBatchTestIDs([]int64{0, 3, 3, -1, 5})
	want := []int64{3, 5}
	if len(got) != len(want) {
		t.Fatalf("len=%d, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestTestAccountForBatchJobSuccess(t *testing.T) {
	handler := &AccountHandler{
		accountTestService: &bulkImportTestRunnerStub{
			result: &service.ScheduledTestResult{
				Status:       "success",
				ResponseText: "OK",
				LatencyMs:    12,
			},
		},
	}

	result := handler.testAccountForBatchJob(context.Background(), 42, "acc")
	if !result.Success || result.AccountID != 42 || result.ResponseText != "OK" || result.LatencyMs != 12 {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestTestAccountForBatchJobFailure(t *testing.T) {
	adminSvc := newStubAdminService()
	handler := &AccountHandler{
		adminService: adminSvc,
		accountTestService: &bulkImportTestRunnerStub{
			result: &service.ScheduledTestResult{
				Status:       "failed",
				ErrorMessage: "invalid credentials",
			},
		},
	}

	result := handler.testAccountForBatchJob(context.Background(), 42, "acc")
	if result.Success || result.Error != "invalid credentials; account disabled" {
		t.Fatalf("unexpected result: %+v", result)
	}
	if adminSvc.updatedAccountStatus[42] != service.StatusDisabled {
		t.Fatalf("expected account 42 disabled, got %q", adminSvc.updatedAccountStatus[42])
	}
	if adminSvc.accountSchedulable[42] {
		t.Fatalf("expected account 42 unschedulable")
	}
	if adminSvc.accountErrors[42] != "account batch test failed: invalid credentials" {
		t.Fatalf("expected batch test error recorded, got %q", adminSvc.accountErrors[42])
	}
}
