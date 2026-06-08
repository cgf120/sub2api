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
	accountBulkImportMaxItems = 1000
	accountBulkImportJobTTL   = 24 * time.Hour
)

type AccountBulkImportItem struct {
	RowNumber  int    `json:"row_number"`
	Name       string `json:"name"`
	Type       string `json:"type"`
	Credential string `json:"credential"`
}

type AccountBulkImportRequest struct {
	Items []AccountBulkImportItem `json:"items" binding:"required,min=1"`
}

type AccountBulkImportResult struct {
	RowNumber int    `json:"row_number"`
	Name      string `json:"name,omitempty"`
	Type      string `json:"type,omitempty"`
	AccountID int64  `json:"account_id,omitempty"`
	Success   bool   `json:"success"`
	Error     string `json:"error,omitempty"`
}

type AccountBulkImportJob struct {
	ID         string                    `json:"id"`
	Status     string                    `json:"status"`
	Total      int                       `json:"total"`
	Processed  int                       `json:"processed"`
	Success    int                       `json:"success"`
	Failed     int                       `json:"failed"`
	Results    []AccountBulkImportResult `json:"results"`
	Error      string                    `json:"error,omitempty"`
	CreatedAt  int64                     `json:"created_at"`
	UpdatedAt  int64                     `json:"updated_at"`
	FinishedAt *int64                    `json:"finished_at,omitempty"`
}

type accountBulkImportJobStore struct {
	mu   sync.RWMutex
	jobs map[string]*AccountBulkImportJob
}

var accountBulkImportJobs = &accountBulkImportJobStore{
	jobs: make(map[string]*AccountBulkImportJob),
}

func (s *accountBulkImportJobStore) create(total int) AccountBulkImportJob {
	now := time.Now().Unix()
	job := &AccountBulkImportJob{
		ID:        uuid.NewString(),
		Status:    "pending",
		Total:     total,
		Results:   make([]AccountBulkImportResult, 0, total),
		CreatedAt: now,
		UpdatedAt: now,
	}

	s.mu.Lock()
	s.pruneLocked(time.Now())
	s.jobs[job.ID] = job
	s.mu.Unlock()

	return cloneAccountBulkImportJob(job)
}

func (s *accountBulkImportJobStore) get(id string) (AccountBulkImportJob, bool) {
	s.mu.RLock()
	job, ok := s.jobs[id]
	if !ok {
		s.mu.RUnlock()
		return AccountBulkImportJob{}, false
	}
	cloned := cloneAccountBulkImportJob(job)
	s.mu.RUnlock()
	return cloned, true
}

func (s *accountBulkImportJobStore) markRunning(id string) {
	s.mu.Lock()
	if job, ok := s.jobs[id]; ok {
		job.Status = "running"
		job.UpdatedAt = time.Now().Unix()
	}
	s.mu.Unlock()
}

func (s *accountBulkImportJobStore) addResult(id string, result AccountBulkImportResult) {
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

func (s *accountBulkImportJobStore) finish(id string, err error) {
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

func (s *accountBulkImportJobStore) pruneLocked(now time.Time) {
	cutoff := now.Add(-accountBulkImportJobTTL).Unix()
	for id, job := range s.jobs {
		if job.UpdatedAt < cutoff {
			delete(s.jobs, id)
		}
	}
}

func cloneAccountBulkImportJob(job *AccountBulkImportJob) AccountBulkImportJob {
	cloned := *job
	cloned.Results = append([]AccountBulkImportResult(nil), job.Results...)
	return cloned
}

// StartBulkImport starts an async GPT/Grok account import job.
// POST /api/v1/admin/accounts/bulk-import
func (h *AccountHandler) StartBulkImport(c *gin.Context) {
	var req AccountBulkImportRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}

	items := normalizeAccountBulkImportItems(req.Items)
	if len(items) == 0 {
		response.BadRequest(c, "No import rows found")
		return
	}
	if len(items) > accountBulkImportMaxItems {
		response.BadRequest(c, fmt.Sprintf("Too many import rows, max is %d", accountBulkImportMaxItems))
		return
	}

	job := accountBulkImportJobs.create(len(items))
	go h.runAccountBulkImportJob(job.ID, items)
	response.Accepted(c, job)
}

// GetBulkImportJob returns async GPT/Grok account import progress.
// GET /api/v1/admin/accounts/bulk-import/:job_id
func (h *AccountHandler) GetBulkImportJob(c *gin.Context) {
	jobID := strings.TrimSpace(c.Param("job_id"))
	if jobID == "" {
		response.BadRequest(c, "Missing job_id")
		return
	}
	job, ok := accountBulkImportJobs.get(jobID)
	if !ok {
		response.Error(c, http.StatusNotFound, "Import job not found")
		return
	}
	response.Success(c, job)
}

func (h *AccountHandler) runAccountBulkImportJob(jobID string, items []AccountBulkImportItem) {
	defer func() {
		if r := recover(); r != nil {
			accountBulkImportJobs.finish(jobID, fmt.Errorf("import job panic: %v", r))
		}
	}()

	accountBulkImportJobs.markRunning(jobID)
	ctx := context.Background()
	for idx, item := range items {
		result := h.createAccountFromBulkImportItem(ctx, item, idx+1)
		accountBulkImportJobs.addResult(jobID, result)
	}
	accountBulkImportJobs.finish(jobID, nil)
}

func normalizeAccountBulkImportItems(items []AccountBulkImportItem) []AccountBulkImportItem {
	normalized := make([]AccountBulkImportItem, 0, len(items))
	for idx, item := range items {
		item.Name = strings.TrimSpace(item.Name)
		item.Type = strings.TrimSpace(item.Type)
		item.Credential = strings.TrimSpace(item.Credential)
		if item.RowNumber <= 0 {
			item.RowNumber = idx + 1
		}
		if item.Name == "" && item.Type == "" && item.Credential == "" {
			continue
		}
		normalized = append(normalized, item)
	}
	return normalized
}

func (h *AccountHandler) createAccountFromBulkImportItem(ctx context.Context, item AccountBulkImportItem, sequence int) AccountBulkImportResult {
	result := AccountBulkImportResult{
		RowNumber: item.RowNumber,
		Type:      item.Type,
	}

	platform, accountType, credentials, defaultPrefix, err := buildAccountBulkImportCreateConfig(item)
	if err != nil {
		result.Error = err.Error()
		return result
	}

	name := strings.TrimSpace(item.Name)
	if name == "" {
		name = fmt.Sprintf("%s #%d", defaultPrefix, sequence)
	}
	result.Name = name

	rateMultiplier := 1.0
	account, err := h.adminService.CreateAccount(ctx, &service.CreateAccountInput{
		Name:           name,
		Platform:       platform,
		Type:           accountType,
		Credentials:    credentials,
		Concurrency:    10,
		Priority:       1,
		RateMultiplier: &rateMultiplier,
	})
	if err != nil {
		result.Error = err.Error()
		return result
	}

	h.scheduleOpenAIResponsesProbe(account)
	result.Success = true
	result.AccountID = account.ID
	return result
}

func buildAccountBulkImportCreateConfig(item AccountBulkImportItem) (platform string, accountType string, credentials map[string]any, defaultPrefix string, err error) {
	credential := strings.TrimSpace(item.Credential)
	if credential == "" {
		return "", "", nil, "", fmt.Errorf("SK/SSO is required")
	}

	switch normalizeAccountBulkImportType(item.Type) {
	case "gpt":
		return service.PlatformOpenAI, service.AccountTypeAPIKey, map[string]any{
			"base_url": "https://api.openai.com",
			"api_key":  credential,
		}, "GPT", nil
	case "grok":
		return service.PlatformGrok, service.AccountTypeOAuth, map[string]any{
			"sso": credential,
		}, "Grok", nil
	default:
		return "", "", nil, "", fmt.Errorf("type must be gpt or grok")
	}
}

func normalizeAccountBulkImportType(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "gpt", "openai", "chatgpt":
		return "gpt"
	case "grok", "xai", "x.ai":
		return "grok"
	default:
		return ""
	}
}
