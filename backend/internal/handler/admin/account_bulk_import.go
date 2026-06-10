package admin

import (
	"context"
	"errors"
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
	accountBulkImportMaxItems          = 1000
	accountBulkImportJobTTL            = 24 * time.Hour
	accountBulkImportValidationTimeout = 75 * time.Second
)

type AccountBulkImportItem struct {
	RowNumber  int    `json:"row_number"`
	Name       string `json:"name"`
	Type       string `json:"type"`
	Credential string `json:"credential"`
}

type AccountBulkImportRequest struct {
	Items    []AccountBulkImportItem `json:"items" binding:"required,min=1"`
	GroupIDs []int64                 `json:"group_ids"`
	ProxyID  *int64                  `json:"proxy_id"`
}

type AccountBulkImportResult struct {
	RowNumber int    `json:"row_number"`
	Name      string `json:"name,omitempty"`
	Type      string `json:"type,omitempty"`
	AccountID int64  `json:"account_id,omitempty"`
	Success   bool   `json:"success"`
	Warning   string `json:"warning,omitempty"`
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

type accountBulkImportOptions struct {
	GroupIDs          []int64
	ProxyID           *int64
	groupPlatformByID map[int64]string
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

// StartBulkImport starts an async GPT/Grok/Gemini/Anthropic account import job.
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
	options, err := h.buildAccountBulkImportOptions(c.Request.Context(), req)
	if err != nil {
		response.BadRequest(c, err.Error())
		return
	}

	job := accountBulkImportJobs.create(len(items))
	go h.runAccountBulkImportJob(job.ID, items, options)
	response.Accepted(c, job)
}

// GetBulkImportJob returns async GPT/Grok/Gemini/Anthropic account import progress.
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

func (h *AccountHandler) runAccountBulkImportJob(jobID string, items []AccountBulkImportItem, options accountBulkImportOptions) {
	defer func() {
		if r := recover(); r != nil {
			accountBulkImportJobs.finish(jobID, fmt.Errorf("import job panic: %v", r))
		}
	}()

	accountBulkImportJobs.markRunning(jobID)
	ctx := context.Background()
	for idx, item := range items {
		result := h.createAccountFromBulkImportItem(ctx, item, idx+1, options)
		accountBulkImportJobs.addResult(jobID, result)
	}
	accountBulkImportJobs.finish(jobID, nil)
}

func (h *AccountHandler) buildAccountBulkImportOptions(ctx context.Context, req AccountBulkImportRequest) (accountBulkImportOptions, error) {
	options := accountBulkImportOptions{
		GroupIDs: normalizeAccountBulkImportGroupIDs(req.GroupIDs),
	}
	if req.ProxyID != nil && *req.ProxyID > 0 {
		proxyID := *req.ProxyID
		options.ProxyID = &proxyID
	}
	if len(options.GroupIDs) == 0 {
		return options, nil
	}
	if h.adminService == nil {
		return options, errors.New("admin service is not configured")
	}
	groups, err := h.adminService.GetAllGroups(ctx)
	if err != nil {
		return options, fmt.Errorf("failed to load groups: %w", err)
	}
	options.groupPlatformByID = make(map[int64]string, len(groups))
	for _, group := range groups {
		options.groupPlatformByID[group.ID] = group.Platform
	}
	for _, groupID := range options.GroupIDs {
		if _, ok := options.groupPlatformByID[groupID]; !ok {
			return options, fmt.Errorf("group %d not found", groupID)
		}
	}
	return options, nil
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

func normalizeAccountBulkImportGroupIDs(groupIDs []int64) []int64 {
	normalized := make([]int64, 0, len(groupIDs))
	seen := make(map[int64]struct{}, len(groupIDs))
	for _, id := range groupIDs {
		if id <= 0 {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		normalized = append(normalized, id)
	}
	return normalized
}

func (h *AccountHandler) createAccountFromBulkImportItem(ctx context.Context, item AccountBulkImportItem, sequence int, options accountBulkImportOptions) AccountBulkImportResult {
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
		ProxyID:        options.ProxyID,
		Concurrency:    10,
		Priority:       1,
		RateMultiplier: &rateMultiplier,
		GroupIDs:       options.groupIDsForPlatform(platform),
	})
	if err != nil {
		result.Error = err.Error()
		return result
	}

	if err := h.validateBulkImportedAccount(ctx, account); err != nil {
		result.AccountID = account.ID
		if isAccountValidationInconclusive(account, err) {
			result.Success = true
			result.Warning = fmt.Sprintf("%s; validation inconclusive, account imported as active", err.Error())
			return result
		}
		if disableErr := h.disableInvalidAccount(ctx, account.ID, err); disableErr != nil {
			result.Error = fmt.Sprintf("%s; failed to disable imported account: %v", err.Error(), disableErr)
			return result
		}
		result.Success = true
		result.Warning = fmt.Sprintf("%s; account imported as disabled", err.Error())
		return result
	}

	h.scheduleOpenAIResponsesProbe(account)
	result.Success = true
	result.AccountID = account.ID
	return result
}

func (o accountBulkImportOptions) groupIDsForPlatform(platform string) []int64 {
	if len(o.GroupIDs) == 0 {
		return nil
	}
	if len(o.groupPlatformByID) == 0 {
		return append([]int64(nil), o.GroupIDs...)
	}
	groupIDs := make([]int64, 0, len(o.GroupIDs))
	for _, groupID := range o.GroupIDs {
		if o.groupPlatformByID[groupID] == platform {
			groupIDs = append(groupIDs, groupID)
		}
	}
	if len(groupIDs) == 0 {
		return nil
	}
	return groupIDs
}

func (h *AccountHandler) disableInvalidAccount(ctx context.Context, accountID int64, validationErr error) error {
	if h.adminService == nil {
		return errors.New("admin service is not configured")
	}
	if validationErr != nil {
		_ = h.adminService.SetAccountError(ctx, accountID, validationErr.Error())
	}
	if _, err := h.adminService.UpdateAccount(ctx, accountID, &service.UpdateAccountInput{Status: service.StatusDisabled}); err != nil {
		return err
	}
	_, _ = h.adminService.SetAccountSchedulable(ctx, accountID, false)
	return nil
}

func (h *AccountHandler) validateBulkImportedAccount(ctx context.Context, account *service.Account) error {
	if account == nil {
		return errors.New("account validation failed: created account is empty")
	}
	if h.accountTestService == nil {
		return errors.New("account validation failed: account test service is not configured")
	}

	testCtx, cancel := context.WithTimeout(ctx, accountBulkImportValidationTimeout)
	defer cancel()

	testResult, err := h.accountTestService.RunTestBackground(testCtx, account.ID, "")
	if err != nil {
		return fmt.Errorf("account validation failed: %w", err)
	}
	if testResult == nil {
		return errors.New("account validation failed: empty test result")
	}
	if strings.EqualFold(testResult.Status, "success") {
		return nil
	}

	message := strings.TrimSpace(testResult.ErrorMessage)
	if message == "" {
		message = "test did not complete successfully"
	}
	return fmt.Errorf("account validation failed: %s", message)
}

func isAccountValidationInconclusive(account *service.Account, err error) bool {
	if account == nil || err == nil || account.Platform != service.PlatformGemini {
		return false
	}
	return isGeminiTransientValidationError(err.Error())
}

func isGeminiTransientValidationError(message string) bool {
	normalized := strings.ToLower(strings.TrimSpace(message))
	if normalized == "" {
		return false
	}
	transientMarkers := []string{
		"api returned 500",
		"api returned 502",
		"api returned 503",
		"api returned 504",
		"status 500",
		"status 502",
		"status 503",
		"status 504",
		"unavailable",
		"currently experiencing high demand",
		"temporarily unavailable",
		"context deadline exceeded",
		"client.timeout",
		"i/o timeout",
		"timeout awaiting response headers",
		"connection reset",
		"connection refused",
		"eof",
	}
	for _, marker := range transientMarkers {
		if strings.Contains(normalized, marker) {
			return true
		}
	}
	return false
}

func buildAccountBulkImportCreateConfig(item AccountBulkImportItem) (platform string, accountType string, credentials map[string]any, defaultPrefix string, err error) {
	credential := strings.TrimSpace(item.Credential)
	if credential == "" {
		return "", "", nil, "", fmt.Errorf("credential is required")
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
	case "gemini":
		return service.PlatformGemini, service.AccountTypeAPIKey, map[string]any{
			"base_url": "https://generativelanguage.googleapis.com",
			"api_key":  credential,
			"tier_id":  service.GeminiTierAIStudioFree,
		}, "Gemini", nil
	case "anthropic":
		return service.PlatformAnthropic, service.AccountTypeAPIKey, map[string]any{
			"base_url": "https://api.anthropic.com",
			"api_key":  credential,
		}, "Anthropic", nil
	default:
		return "", "", nil, "", fmt.Errorf("type must be gpt, grok, gemini, or anthropic")
	}
}

func normalizeAccountBulkImportType(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "gpt", "openai", "chatgpt":
		return "gpt"
	case "grok", "xai", "x.ai":
		return "grok"
	case "gemini", "google", "google-ai", "google_ai", "ai-studio", "ai_studio", "aistudio":
		return "gemini"
	case "anthropic", "claude", "claude-api", "claude_api":
		return "anthropic"
	default:
		return ""
	}
}
