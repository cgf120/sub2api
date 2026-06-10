package admin

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const (
	accountBatchTestMaxItems      = 500
	accountBatchTestJobTTL        = 24 * time.Hour
	accountBatchTestItemTimeout   = 90 * time.Second
	accountBatchTestMaxConcurrent = 3
)

type AccountBatchTestRequest struct {
	AccountIDs []int64 `json:"account_ids" binding:"required,min=1"`
}

type AccountBatchTestResult struct {
	AccountID    int64  `json:"account_id"`
	Name         string `json:"name,omitempty"`
	Success      bool   `json:"success"`
	Error        string `json:"error,omitempty"`
	ResponseText string `json:"response_text,omitempty"`
	LatencyMs    int64  `json:"latency_ms,omitempty"`
}

type AccountBatchTestJob struct {
	ID         string                   `json:"id"`
	Status     string                   `json:"status"`
	Total      int                      `json:"total"`
	Processed  int                      `json:"processed"`
	Success    int                      `json:"success"`
	Failed     int                      `json:"failed"`
	Results    []AccountBatchTestResult `json:"results"`
	Error      string                   `json:"error,omitempty"`
	CreatedAt  int64                    `json:"created_at"`
	UpdatedAt  int64                    `json:"updated_at"`
	FinishedAt *int64                   `json:"finished_at,omitempty"`
}

type accountBatchTestJobStore struct {
	mu   sync.RWMutex
	jobs map[string]*AccountBatchTestJob
}

var accountBatchTestJobs = &accountBatchTestJobStore{
	jobs: make(map[string]*AccountBatchTestJob),
}

func (s *accountBatchTestJobStore) create(total int) AccountBatchTestJob {
	now := time.Now().Unix()
	job := &AccountBatchTestJob{
		ID:        uuid.NewString(),
		Status:    "pending",
		Total:     total,
		Results:   make([]AccountBatchTestResult, 0, total),
		CreatedAt: now,
		UpdatedAt: now,
	}

	s.mu.Lock()
	s.pruneLocked(time.Now())
	s.jobs[job.ID] = job
	s.mu.Unlock()

	return cloneAccountBatchTestJob(job)
}

func (s *accountBatchTestJobStore) get(id string) (AccountBatchTestJob, bool) {
	s.mu.RLock()
	job, ok := s.jobs[id]
	if !ok {
		s.mu.RUnlock()
		return AccountBatchTestJob{}, false
	}
	cloned := cloneAccountBatchTestJob(job)
	s.mu.RUnlock()
	return cloned, true
}

func (s *accountBatchTestJobStore) markRunning(id string) {
	s.mu.Lock()
	if job, ok := s.jobs[id]; ok {
		job.Status = "running"
		job.UpdatedAt = time.Now().Unix()
	}
	s.mu.Unlock()
}

func (s *accountBatchTestJobStore) addResult(id string, result AccountBatchTestResult) {
	s.mu.Lock()
	if job, ok := s.jobs[id]; ok {
		job.Processed++
		if result.Success {
			job.Success++
		} else {
			job.Failed++
		}
		job.Results = append(job.Results, result)
		job.UpdatedAt = time.Now().Unix()
	}
	s.mu.Unlock()
}

func (s *accountBatchTestJobStore) finish(id string, err error) {
	s.mu.Lock()
	if job, ok := s.jobs[id]; ok {
		now := time.Now().Unix()
		if err != nil {
			job.Status = "failed"
			job.Error = err.Error()
		} else {
			job.Status = "completed"
		}
		job.UpdatedAt = now
		job.FinishedAt = &now
	}
	s.mu.Unlock()
}

func (s *accountBatchTestJobStore) pruneLocked(now time.Time) {
	cutoff := now.Add(-accountBatchTestJobTTL).Unix()
	for id, job := range s.jobs {
		if job.UpdatedAt < cutoff {
			delete(s.jobs, id)
		}
	}
}

func cloneAccountBatchTestJob(job *AccountBatchTestJob) AccountBatchTestJob {
	cloned := *job
	cloned.Results = append([]AccountBatchTestResult(nil), job.Results...)
	return cloned
}

// StartBatchTest starts an async selected-account test job.
// POST /api/v1/admin/accounts/batch-test
func (h *AccountHandler) StartBatchTest(c *gin.Context) {
	var req AccountBatchTestRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}

	accountIDs := normalizeAccountBatchTestIDs(req.AccountIDs)
	if len(accountIDs) == 0 {
		response.BadRequest(c, "account_ids is required")
		return
	}
	if len(accountIDs) > accountBatchTestMaxItems {
		response.BadRequest(c, fmt.Sprintf("Too many accounts, max is %d", accountBatchTestMaxItems))
		return
	}
	if h.accountTestService == nil {
		response.Error(c, http.StatusServiceUnavailable, "Account test service is not configured")
		return
	}

	job := accountBatchTestJobs.create(len(accountIDs))
	go h.runAccountBatchTestJob(job.ID, accountIDs)
	response.Accepted(c, job)
}

// GetBatchTestJob returns async selected-account test progress.
// GET /api/v1/admin/accounts/batch-test/:job_id
func (h *AccountHandler) GetBatchTestJob(c *gin.Context) {
	jobID := strings.TrimSpace(c.Param("job_id"))
	if jobID == "" {
		response.BadRequest(c, "Missing job_id")
		return
	}
	job, ok := accountBatchTestJobs.get(jobID)
	if !ok {
		response.Error(c, http.StatusNotFound, "Batch test job not found")
		return
	}
	response.Success(c, job)
}

func normalizeAccountBatchTestIDs(ids []int64) []int64 {
	seen := make(map[int64]struct{}, len(ids))
	out := make([]int64, 0, len(ids))
	for _, id := range ids {
		if id <= 0 {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

func (h *AccountHandler) runAccountBatchTestJob(jobID string, accountIDs []int64) {
	defer func() {
		if r := recover(); r != nil {
			accountBatchTestJobs.finish(jobID, fmt.Errorf("batch test job panic: %v", r))
		}
	}()

	accountBatchTestJobs.markRunning(jobID)
	ctx := context.Background()
	accounts, err := h.adminService.GetAccountsByIDs(ctx, accountIDs)
	if err != nil {
		accountBatchTestJobs.finish(jobID, err)
		return
	}

	accountByID := make(map[int64]*service.Account, len(accounts))
	for _, account := range accounts {
		if account != nil {
			accountByID[account.ID] = account
		}
	}

	sem := make(chan struct{}, accountBatchTestMaxConcurrent)
	var wg sync.WaitGroup
	for _, id := range accountIDs {
		accountID := id
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer func() {
				<-sem
				wg.Done()
			}()
			result := h.testAccountForBatchJob(ctx, accountID, accountByID[accountID])
			accountBatchTestJobs.addResult(jobID, result)
		}()
	}
	wg.Wait()
	accountBatchTestJobs.finish(jobID, nil)
}

func (h *AccountHandler) testAccountForBatchJob(ctx context.Context, accountID int64, account *service.Account) AccountBatchTestResult {
	result := AccountBatchTestResult{
		AccountID: accountID,
	}
	if account == nil {
		result.Error = "account not found"
		return result
	}
	result.Name = account.Name

	testCtx, cancel := context.WithTimeout(ctx, accountBatchTestItemTimeout)
	defer cancel()

	testResult, err := h.accountTestService.RunTestBackground(testCtx, accountID, "")
	if err != nil {
		result.Error = err.Error()
		if isAccountValidationInconclusive(account, err) {
			result.Error = result.Error + "; account unchanged"
			return result
		}
		h.disableFailedBatchTestAccount(ctx, accountID, &result)
		return result
	}
	if testResult == nil {
		result.Error = "empty test result"
		h.disableFailedBatchTestAccount(ctx, accountID, &result)
		return result
	}

	result.ResponseText = testResult.ResponseText
	result.LatencyMs = testResult.LatencyMs
	if strings.EqualFold(testResult.Status, "success") {
		result.Success = true
		if h.rateLimitService != nil {
			_, _ = h.rateLimitService.RecoverAccountAfterSuccessfulTest(ctx, accountID)
		}
		return result
	}

	result.Error = strings.TrimSpace(testResult.ErrorMessage)
	if result.Error == "" {
		result.Error = "test failed"
	}
	if isAccountValidationInconclusive(account, fmt.Errorf("%s", result.Error)) {
		result.Error = result.Error + "; account unchanged"
		return result
	}
	h.disableFailedBatchTestAccount(ctx, accountID, &result)
	return result
}

func (h *AccountHandler) disableFailedBatchTestAccount(ctx context.Context, accountID int64, result *AccountBatchTestResult) {
	if result == nil || result.Success || result.Error == "" {
		return
	}
	disableErr := h.disableInvalidAccount(ctx, accountID, fmt.Errorf("account batch test failed: %s", result.Error))
	if disableErr != nil {
		result.Error = fmt.Sprintf("%s; failed to disable account: %v", result.Error, disableErr)
		return
	}
	result.Error = result.Error + "; account disabled"
}
