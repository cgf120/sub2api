package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/grok"
	"github.com/google/uuid"
)

type GrokGatewayService struct {
	accountRepo        AccountRepository
	concurrencyService *ConcurrencyService
	budgetCache        GrokBudgetCache
	cfg                *config.Config
	roundRobinMu       sync.Mutex
	roundRobinCursor   map[string]int64
}

const (
	grokAntiBotCooldown        = 30 * time.Minute
	grokInvalidSessionCooldown = 24 * time.Hour
	grokRateLimitCooldown      = 2 * time.Hour
	grokTransientCooldown      = 2 * time.Minute
	grokTextMaxConcurrency     = 2

	grokRoundRobinScopeText = "grok:text"
)

func NewGrokGatewayService(accountRepo AccountRepository, concurrencyService *ConcurrencyService, budgetCache GrokBudgetCache, cfg *config.Config) *GrokGatewayService {
	return &GrokGatewayService{
		accountRepo:        accountRepo,
		concurrencyService: concurrencyService,
		budgetCache:        budgetCache,
		cfg:                cfg,
		roundRobinCursor:   make(map[string]int64),
	}
}

type GrokChatCompletionRequest struct {
	Model  string
	Stream bool
	Prompt string
	ModeID string
}

type GrokImagesRequest struct {
	Model       string
	Prompt      string
	Count       int
	Size        string
	Quality     string
	AspectRatio string
}

type GrokVideoRequest struct {
	Model          string
	Prompt         string
	Seconds        int
	Size           string
	AspectRatio    string
	ResolutionName string
	Preset         string
}

func (s *GrokGatewayService) SelectAccount(ctx context.Context, groupID *int64, requestedModel string, excludedIDs map[int64]struct{}) (*Account, error) {
	if s == nil || s.accountRepo == nil {
		return nil, errors.New("grok gateway service is not configured")
	}
	candidates, err := s.grokAccountCandidates(ctx, groupID, requestedModel, excludedIDs)
	if err != nil {
		return nil, err
	}
	if len(candidates) == 0 {
		return nil, errors.New("no available Grok accounts")
	}
	ordered := s.orderGrokAccountsByRoundRobin(groupID, requestedModel, candidates)
	return ordered[0], nil
}

func (s *GrokGatewayService) SelectAccountWithLoadAwareness(ctx context.Context, groupID *int64, requestedModel string, excludedIDs map[int64]struct{}) (*AccountSelectionResult, error) {
	if s == nil || s.accountRepo == nil {
		return nil, errors.New("grok gateway service is not configured")
	}
	candidates, err := s.grokAccountCandidates(ctx, groupID, requestedModel, excludedIDs)
	if err != nil {
		return nil, err
	}
	if len(candidates) == 0 {
		if strings.TrimSpace(requestedModel) != "" {
			return nil, fmt.Errorf("no available Grok accounts supporting model: %s", requestedModel)
		}
		return nil, errors.New("no available Grok accounts")
	}

	if s.concurrencyService == nil || !s.grokLoadBatchEnabled() {
		return s.selectAndAcquireGrokAccount(ctx, groupID, candidates, requestedModel)
	}

	accountLoads := make([]AccountWithConcurrency, 0, len(candidates))
	for _, acc := range candidates {
		accountLoads = append(accountLoads, AccountWithConcurrency{
			ID:             acc.ID,
			MaxConcurrency: grokAccountMaxConcurrency(acc, requestedModel),
		})
	}

	loadMap, err := s.concurrencyService.GetAccountsLoadBatch(ctx, accountLoads)
	if err != nil {
		return s.selectAndAcquireGrokAccount(ctx, groupID, candidates, requestedModel)
	}

	available := make([]accountWithLoad, 0, len(candidates))
	for _, acc := range candidates {
		loadInfo := loadMap[acc.ID]
		if loadInfo == nil {
			loadInfo = &AccountLoadInfo{AccountID: acc.ID}
		}
		if loadInfo.LoadRate < 100 {
			available = append(available, accountWithLoad{account: acc, loadInfo: loadInfo})
		}
	}
	if len(available) > 0 {
		ordered := s.orderGrokAccountLoadsByRoundRobin(groupID, requestedModel, available)
		for _, item := range ordered {
			if !isGrokAccountEligibleForModel(ctx, item.account, requestedModel) {
				continue
			}
			result, err := s.tryAcquireGrokAccountSlot(ctx, item.account, requestedModel)
			if err == nil && result != nil && result.Acquired {
				if ok, _ := s.reserveGrokBudget(ctx, item.account, requestedModel); !ok {
					if result.ReleaseFunc != nil {
						result.ReleaseFunc()
					}
					continue
				}
				return &AccountSelectionResult{
					Account:     item.account,
					Acquired:    true,
					ReleaseFunc: result.ReleaseFunc,
				}, nil
			}
		}
	}

	return s.waitPlanForGrokAccounts(candidates, requestedModel), nil
}

func (s *GrokGatewayService) grokAccountCandidates(ctx context.Context, groupID *int64, requestedModel string, excludedIDs map[int64]struct{}) ([]*Account, error) {
	accounts, err := s.listSchedulableAccounts(ctx, groupID)
	if err != nil {
		return nil, err
	}
	candidates := make([]*Account, 0, len(accounts))
	for i := range accounts {
		acc := &accounts[i]
		if excludedIDs != nil {
			if _, ok := excludedIDs[acc.ID]; ok {
				continue
			}
		}
		if !isGrokAccountEligibleForModel(ctx, acc, requestedModel) {
			continue
		}
		if !s.isGrokBudgetAvailable(ctx, acc, requestedModel) {
			continue
		}
		candidates = append(candidates, acc)
	}
	return candidates, nil
}

func isGrokAccountEligibleForModel(ctx context.Context, acc *Account, requestedModel string) bool {
	if acc == nil || !acc.IsSchedulable() || acc.Platform != PlatformGrok || !acc.IsModelSupported(requestedModel) {
		return false
	}
	if acc.GetRateLimitRemainingTimeWithContext(ctx, requestedModel) > 0 {
		return false
	}
	if grokSSOFromAccount(acc) == "" {
		return false
	}
	return true
}

func (s *GrokGatewayService) selectAndAcquireGrokAccount(ctx context.Context, groupID *int64, candidates []*Account, requestedModel string) (*AccountSelectionResult, error) {
	ordered := s.orderGrokAccountsByRoundRobin(groupID, requestedModel, candidates)
	for _, acc := range ordered {
		if !isGrokAccountEligibleForModel(ctx, acc, requestedModel) {
			continue
		}
		result, err := s.tryAcquireGrokAccountSlot(ctx, acc, requestedModel)
		if err == nil && result != nil && result.Acquired {
			if ok, _ := s.reserveGrokBudget(ctx, acc, requestedModel); !ok {
				if result.ReleaseFunc != nil {
					result.ReleaseFunc()
				}
				continue
			}
			return &AccountSelectionResult{Account: acc, Acquired: true, ReleaseFunc: result.ReleaseFunc}, nil
		}
	}
	return s.waitPlanForGrokAccounts(ordered, requestedModel), nil
}

func (s *GrokGatewayService) waitPlanForGrokAccounts(candidates []*Account, requestedModel string) *AccountSelectionResult {
	if len(candidates) == 0 {
		return nil
	}
	ordered := append([]*Account(nil), candidates...)
	sortAccountsByPriorityAndLastUsed(ordered, false)
	account := ordered[0]
	cfg := s.grokSchedulingConfig()
	return &AccountSelectionResult{
		Account: account,
		WaitPlan: &AccountWaitPlan{
			AccountID:      account.ID,
			MaxConcurrency: grokAccountMaxConcurrency(account, requestedModel),
			Timeout:        cfg.FallbackWaitTimeout,
			MaxWaiting:     cfg.FallbackMaxWaiting,
		},
	}
}

func (s *GrokGatewayService) tryAcquireGrokAccountSlot(ctx context.Context, account *Account, requestedModel string) (*AcquireResult, error) {
	if s == nil || s.concurrencyService == nil {
		return &AcquireResult{Acquired: true, ReleaseFunc: func() {}}, nil
	}
	return s.concurrencyService.AcquireAccountSlot(ctx, account.ID, grokAccountMaxConcurrency(account, requestedModel))
}

func (s *GrokGatewayService) grokLoadBatchEnabled() bool {
	cfg := s.grokSchedulingConfig()
	return cfg.LoadBatchEnabled
}

func (s *GrokGatewayService) grokSchedulingConfig() config.GatewaySchedulingConfig {
	if s != nil && s.cfg != nil {
		return s.cfg.Gateway.Scheduling
	}
	return config.GatewaySchedulingConfig{
		FallbackWaitTimeout: 30 * time.Second,
		FallbackMaxWaiting:  100,
		LoadBatchEnabled:    true,
	}
}

func grokAccountMaxConcurrency(acc *Account, requestedModel string) int {
	if acc == nil {
		return 1
	}
	configured := acc.Concurrency
	if acc.LoadFactor != nil && *acc.LoadFactor > 0 {
		configured = *acc.LoadFactor
	}
	model := strings.TrimSpace(requestedModel)
	switch model {
	case grok.ModelImageLite:
		return clampGrokMediaConcurrency(configured, 2)
	case grok.ModelImage, grok.ModelImagePro, grok.ModelImagineVideo:
		return clampGrokMediaConcurrency(configured, 1)
	default:
		return clampGrokMediaConcurrency(configured, grokTextMaxConcurrency)
	}
}

func clampGrokMediaConcurrency(value, max int) int {
	if value <= 0 {
		return max
	}
	if value > max {
		return max
	}
	return value
}

func (s *GrokGatewayService) orderGrokAccountsByRoundRobin(groupID *int64, requestedModel string, accounts []*Account) []*Account {
	ordered := append([]*Account(nil), accounts...)
	if len(ordered) <= 1 {
		return ordered
	}
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].Priority != ordered[j].Priority {
			return ordered[i].Priority < ordered[j].Priority
		}
		return ordered[i].ID < ordered[j].ID
	})

	baseKey := grokRoundRobinBaseKey(groupID, requestedModel)
	out := make([]*Account, 0, len(ordered))
	for i := 0; i < len(ordered); {
		j := i + 1
		for j < len(ordered) && ordered[i].Priority == ordered[j].Priority {
			j++
		}
		key := fmt.Sprintf("%s:p:%d", baseKey, ordered[i].Priority)
		out = append(out, s.rotateGrokAccountGroup(key, ordered[i:j])...)
		i = j
	}
	return out
}

func (s *GrokGatewayService) orderGrokAccountLoadsByRoundRobin(groupID *int64, requestedModel string, accounts []accountWithLoad) []accountWithLoad {
	ordered := append([]accountWithLoad(nil), accounts...)
	if len(ordered) <= 1 {
		return ordered
	}
	sort.SliceStable(ordered, func(i, j int) bool {
		a, b := ordered[i], ordered[j]
		if a.account.Priority != b.account.Priority {
			return a.account.Priority < b.account.Priority
		}
		aBucket := grokLoadBucket(a.loadInfo)
		bBucket := grokLoadBucket(b.loadInfo)
		if aBucket != bBucket {
			return aBucket < bBucket
		}
		return a.account.ID < b.account.ID
	})

	baseKey := grokRoundRobinBaseKey(groupID, requestedModel)
	out := make([]accountWithLoad, 0, len(ordered))
	for i := 0; i < len(ordered); {
		j := i + 1
		priority := ordered[i].account.Priority
		bucket := grokLoadBucket(ordered[i].loadInfo)
		for j < len(ordered) && ordered[j].account.Priority == priority && grokLoadBucket(ordered[j].loadInfo) == bucket {
			j++
		}
		key := fmt.Sprintf("%s:p:%d:b:%d", baseKey, priority, bucket)
		out = append(out, s.rotateGrokAccountLoadGroup(key, ordered[i:j])...)
		i = j
	}
	return out
}

func (s *GrokGatewayService) rotateGrokAccountGroup(key string, accounts []*Account) []*Account {
	if len(accounts) <= 1 {
		return append([]*Account(nil), accounts...)
	}
	lastID := s.claimNextGrokRoundRobinStart(key, grokAccountGroupIDs(accounts))
	start := grokRoundRobinStartIndex(lastID, grokAccountGroupIDs(accounts))
	rotated := make([]*Account, 0, len(accounts))
	rotated = append(rotated, accounts[start:]...)
	rotated = append(rotated, accounts[:start]...)
	return rotated
}

func (s *GrokGatewayService) rotateGrokAccountLoadGroup(key string, accounts []accountWithLoad) []accountWithLoad {
	if len(accounts) <= 1 {
		return append([]accountWithLoad(nil), accounts...)
	}
	lastID := s.claimNextGrokRoundRobinStart(key, grokAccountLoadGroupIDs(accounts))
	start := grokRoundRobinStartIndex(lastID, grokAccountLoadGroupIDs(accounts))
	rotated := make([]accountWithLoad, 0, len(accounts))
	rotated = append(rotated, accounts[start:]...)
	rotated = append(rotated, accounts[:start]...)
	return rotated
}

func (s *GrokGatewayService) claimNextGrokRoundRobinStart(key string, ids []int64) int64 {
	if len(ids) == 0 {
		return 0
	}
	if s == nil {
		return ids[0]
	}
	s.roundRobinMu.Lock()
	defer s.roundRobinMu.Unlock()
	if s.roundRobinCursor == nil {
		s.roundRobinCursor = make(map[string]int64)
	}
	lastServed := s.roundRobinCursor[key]
	start := grokRoundRobinStartIndex(lastServed, ids)
	claimed := ids[start]
	s.roundRobinCursor[key] = claimed
	return lastServed
}

func grokRoundRobinStartIndex(lastID int64, ids []int64) int {
	if len(ids) == 0 {
		return 0
	}
	for i, id := range ids {
		if id == lastID {
			return (i + 1) % len(ids)
		}
	}
	return 0
}

func grokAccountGroupIDs(accounts []*Account) []int64 {
	ids := make([]int64, 0, len(accounts))
	for _, account := range accounts {
		if account != nil {
			ids = append(ids, account.ID)
		}
	}
	return ids
}

func grokAccountLoadGroupIDs(accounts []accountWithLoad) []int64 {
	ids := make([]int64, 0, len(accounts))
	for _, item := range accounts {
		if item.account != nil {
			ids = append(ids, item.account.ID)
		}
	}
	return ids
}

func grokRoundRobinBaseKey(groupID *int64, requestedModel string) string {
	group := "ungrouped"
	if groupID != nil {
		group = strconv.FormatInt(*groupID, 10)
	}
	return "group:" + group + ":scope:" + grokRoundRobinScopeForModel(requestedModel)
}

func grokRoundRobinScopeForModel(model string) string {
	if scope := grokBudgetScopeForModel(strings.TrimSpace(model)); scope != "" {
		return scope
	}
	return grokRoundRobinScopeText
}

func grokLoadBucket(loadInfo *AccountLoadInfo) int {
	if loadInfo == nil || loadInfo.LoadRate <= 0 {
		return 0
	}
	if loadInfo.LoadRate >= 100 {
		return 4
	}
	return loadInfo.LoadRate / 25
}

func (s *GrokGatewayService) listSchedulableAccounts(ctx context.Context, groupID *int64) ([]Account, error) {
	var (
		accounts []Account
		err      error
	)
	if s.cfg != nil && s.cfg.RunMode == config.RunModeSimple {
		accounts, err = s.accountRepo.ListSchedulableByPlatform(ctx, PlatformGrok)
	} else if groupID != nil {
		accounts, err = s.accountRepo.ListSchedulableByGroupIDAndPlatform(ctx, *groupID, PlatformGrok)
	} else {
		accounts, err = s.accountRepo.ListSchedulableUngroupedByPlatform(ctx, PlatformGrok)
	}
	if err != nil {
		return nil, fmt.Errorf("query grok accounts failed: %w", err)
	}
	return accounts, nil
}

func (s *GrokGatewayService) ClientForAccount(account *Account) (*grok.Client, error) {
	if account == nil {
		return nil, errors.New("account is nil")
	}
	cfg := grok.Config{
		SSO:          grokSSOFromAccount(account),
		UserAgent:    firstCredential(account, "user_agent", "ua"),
		ExtraCookies: buildGrokExtraCookies(account),
	}
	if account.Proxy != nil && account.Proxy.IsActive() {
		cfg.ProxyURL = account.Proxy.URL()
	}
	return grok.NewClient(cfg)
}

func (s *GrokGatewayService) MarkAntiBotTemporary(ctx context.Context, groupID *int64, model string, account *Account, err error) {
	if s == nil || s.accountRepo == nil || account == nil || !grok.IsAntiBotError(err) {
		return
	}
	_, selectErr := s.SelectAccount(ctx, groupID, model, map[int64]struct{}{account.ID: struct{}{}})
	if selectErr != nil {
		slog.Info("grok.antibot.temp_unschedulable_skipped_no_fallback",
			"account_id", account.ID,
			"model", model,
			"error", selectErr,
		)
		return
	}
	until := time.Now().Add(grokAntiBotCooldown)
	reason := "grok anti-bot 403 code=7"
	if setErr := s.accountRepo.SetTempUnschedulable(ctx, account.ID, until, reason); setErr != nil {
		slog.Warn("grok.antibot.set_temp_unschedulable_failed",
			"account_id", account.ID,
			"until", until,
			"error", setErr,
		)
		return
	}
	slog.Info("grok.antibot.temp_unschedulable_set",
		"account_id", account.ID,
		"until", until,
		"reason", reason,
	)
}

func (s *GrokGatewayService) MarkUpstreamFailure(ctx context.Context, model string, account *Account, err error) {
	if s == nil || s.accountRepo == nil || account == nil || err == nil {
		return
	}
	now := time.Now()
	switch {
	case grok.IsInvalidSessionError(err):
		reason := "grok invalid session"
		_ = s.accountRepo.SetError(ctx, account.ID, err.Error())
		s.setTempUnschedulable(ctx, account.ID, now.Add(grokInvalidSessionCooldown), reason)
	case grok.IsAntiBotError(err):
		s.setTempUnschedulable(ctx, account.ID, now.Add(grokAntiBotCooldown), "grok anti-bot 403 code=7")
	case grok.IsRateLimitError(err):
		scope := strings.TrimSpace(model)
		if scope == "" {
			scope = "grok"
		}
		reason := "grok upstream rate limit"
		if grok.IsImageModel(scope) {
			reason = "grok image rate limit"
		} else if grok.IsVideoModel(scope) {
			reason = "grok video rate limit"
		}
		resetAt := now.Add(grokRateLimitCooldown)
		if setErr := s.accountRepo.SetModelRateLimit(ctx, account.ID, scope, resetAt, reason); setErr != nil {
			slog.Warn("grok.set_model_rate_limit_failed",
				"account_id", account.ID,
				"model", scope,
				"reset_at", resetAt,
				"error", setErr,
			)
		}
	case grok.IsTransientDisconnectError(err):
		s.setTempUnschedulable(ctx, account.ID, now.Add(grokTransientCooldown), "grok transient disconnect")
	}
}

func (s *GrokGatewayService) setTempUnschedulable(ctx context.Context, accountID int64, until time.Time, reason string) {
	if err := s.accountRepo.SetTempUnschedulable(ctx, accountID, until, reason); err != nil {
		slog.Warn("grok.set_temp_unschedulable_failed",
			"account_id", accountID,
			"until", until,
			"reason", reason,
			"error", err,
		)
	}
}

func (s *GrokGatewayService) Chat(ctx context.Context, account *Account, req *GrokChatCompletionRequest, onToken func(string) error) (*OpenAIForwardResult, string, error) {
	if req == nil {
		return nil, "", errors.New("request is nil")
	}
	client, err := s.ClientForAccount(account)
	if err != nil {
		return nil, "", err
	}
	model := strings.TrimSpace(req.Model)
	if model == "" {
		model = grok.ModelChatFast
	}
	upstreamModel, _ := account.ResolveMappedModel(model)
	modeID := strings.TrimSpace(req.ModeID)
	if modeID == "" {
		modeID = firstCredential(account, "mode_id", "mode")
	}
	if modeID == "" {
		modeID = grok.ModeIDForModel(upstreamModel)
	}

	start := time.Now()
	result, err := client.Chat(ctx, grok.ChatRequest{Prompt: req.Prompt, ModeID: modeID}, onToken)
	if err != nil {
		return nil, "", err
	}
	text := ""
	var firstTokenMS *int
	if result != nil {
		text = result.Text
		firstTokenMS = result.FirstTokenMS
	}
	forwardResult := &OpenAIForwardResult{
		RequestID:     "grok-" + uuid.NewString(),
		ResponseID:    "chatcmpl-" + uuid.NewString(),
		Model:         model,
		UpstreamModel: optionalUpstreamModel(model, upstreamModel),
		Usage: OpenAIUsage{
			InputTokens:  estimateTokens(req.Prompt),
			OutputTokens: estimateTokens(text),
		},
		Stream:       req.Stream,
		Duration:     time.Since(start),
		FirstTokenMs: firstTokenMS,
	}
	return forwardResult, text, nil
}

func (s *GrokGatewayService) Images(ctx context.Context, account *Account, req *GrokImagesRequest) (*OpenAIForwardResult, []grok.ImageResult, error) {
	if req == nil {
		return nil, nil, errors.New("request is nil")
	}
	client, err := s.ClientForAccount(account)
	if err != nil {
		return nil, nil, err
	}
	model := strings.TrimSpace(req.Model)
	if model == "" {
		model = grok.ModelImage
	}
	upstreamModel, _ := account.ResolveMappedModel(model)
	count := req.Count
	if count <= 0 {
		count = 1
	}
	aspectRatio := strings.TrimSpace(req.AspectRatio)
	if aspectRatio == "" {
		aspectRatio = aspectRatioFromSize(req.Size)
	}
	enablePro := strings.Contains(strings.ToLower(upstreamModel), "pro") || strings.EqualFold(req.Quality, "hd")
	start := time.Now()
	imageReq := grok.ImageRequest{
		Prompt:      req.Prompt,
		Count:       count,
		AspectRatio: aspectRatio,
		EnableNSFW:  true,
		EnablePro:   enablePro,
	}
	var images []grok.ImageResult
	if strings.EqualFold(upstreamModel, grok.ModelImageLite) || strings.EqualFold(model, grok.ModelImageLite) {
		images, err = client.GenerateLiteImages(ctx, imageReq)
	} else {
		images, err = client.GenerateImages(ctx, imageReq)
	}
	if err != nil {
		return nil, nil, err
	}
	sizeTier := NormalizeImageBillingTierOrDefault(req.Size)
	result := &OpenAIForwardResult{
		RequestID:     "grok-" + uuid.NewString(),
		ResponseID:    "img-" + uuid.NewString(),
		Model:         model,
		UpstreamModel: optionalUpstreamModel(model, upstreamModel),
		Usage: OpenAIUsage{
			InputTokens:  estimateTokens(req.Prompt),
			OutputTokens: len(images),
		},
		Duration:   time.Since(start),
		ImageCount: len(images),
		ImageSize:  sizeTier,
	}
	return result, images, nil
}

func (s *GrokGatewayService) Video(ctx context.Context, account *Account, req *GrokVideoRequest, onProgress func(int) error) (*OpenAIForwardResult, *grok.VideoResult, error) {
	if req == nil {
		return nil, nil, errors.New("request is nil")
	}
	client, err := s.ClientForAccount(account)
	if err != nil {
		return nil, nil, err
	}
	model := strings.TrimSpace(req.Model)
	if model == "" {
		model = grok.ModelImagineVideo
	}
	upstreamModel, _ := account.ResolveMappedModel(model)
	aspectRatio := strings.TrimSpace(req.AspectRatio)
	if aspectRatio == "" {
		aspectRatio = aspectRatioFromSize(req.Size)
	}
	if aspectRatio == "" {
		aspectRatio = "9:16"
	}
	start := time.Now()
	videoSeconds := req.Seconds
	if videoSeconds <= 0 {
		videoSeconds = 6
	}
	video, err := client.GenerateVideo(ctx, grok.VideoRequest{
		Prompt:         req.Prompt,
		AspectRatio:    aspectRatio,
		ResolutionName: strings.TrimSpace(req.ResolutionName),
		Seconds:        videoSeconds,
		Preset:         req.Preset,
	}, onProgress)
	if err != nil {
		return nil, nil, err
	}
	result := &OpenAIForwardResult{
		RequestID:     "grok-" + uuid.NewString(),
		ResponseID:    "video-" + uuid.NewString(),
		Model:         model,
		UpstreamModel: optionalUpstreamModel(model, upstreamModel),
		Usage: OpenAIUsage{
			InputTokens:  estimateTokens(req.Prompt),
			OutputTokens: videoSeconds,
		},
		Duration: time.Since(start),
	}
	return result, video, nil
}

func (s *GrokGatewayService) TestConnection(ctx context.Context, account *Account, modelID string) (string, error) {
	model := strings.TrimSpace(modelID)
	if model == "" {
		model = grok.ModelChatFast
	}
	req := &GrokChatCompletionRequest{
		Model:  model,
		Prompt: "请只回复 OK",
		ModeID: grok.ModeIDForModel(model),
	}
	_, text, err := s.Chat(ctx, account, req, nil)
	return text, err
}

func (s *GrokGatewayService) FetchRateLimits(ctx context.Context, account *Account, modelID string) (*grok.RateLimitSnapshot, error) {
	client, err := s.ClientForAccount(account)
	if err != nil {
		return nil, err
	}
	model := strings.TrimSpace(modelID)
	if model == "" {
		model = grok.ModelChatFast
	}
	upstreamModel, _ := account.ResolveMappedModel(model)
	return client.FetchRateLimits(ctx, grok.ModeIDForModel(upstreamModel), "")
}

func ParseGrokChatCompletionRequest(body []byte) (*GrokChatCompletionRequest, error) {
	var raw struct {
		Model    string `json:"model"`
		Stream   bool   `json:"stream"`
		ModeID   string `json:"mode_id"`
		Messages []struct {
			Role    string `json:"role"`
			Content any    `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}
	prompt := flattenChatMessages(raw.Messages)
	return &GrokChatCompletionRequest{
		Model:  strings.TrimSpace(raw.Model),
		Stream: raw.Stream,
		Prompt: prompt,
		ModeID: strings.TrimSpace(raw.ModeID),
	}, nil
}

func ParseGrokImagesRequest(body []byte) (*GrokImagesRequest, error) {
	var raw struct {
		Model       string `json:"model"`
		Prompt      string `json:"prompt"`
		N           int    `json:"n"`
		Size        string `json:"size"`
		Quality     string `json:"quality"`
		AspectRatio string `json:"aspect_ratio"`
		Stream      bool   `json:"stream"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}
	if raw.Stream {
		return nil, errors.New("streaming image generation is not supported for Grok")
	}
	return &GrokImagesRequest{
		Model:       strings.TrimSpace(raw.Model),
		Prompt:      strings.TrimSpace(raw.Prompt),
		Count:       raw.N,
		Size:        strings.TrimSpace(raw.Size),
		Quality:     strings.TrimSpace(raw.Quality),
		AspectRatio: strings.TrimSpace(raw.AspectRatio),
	}, nil
}

func ParseGrokVideoRequest(body []byte) (*GrokVideoRequest, error) {
	var raw struct {
		Model          string `json:"model"`
		Prompt         string `json:"prompt"`
		Seconds        int    `json:"seconds"`
		Duration       int    `json:"duration"`
		VideoLength    int    `json:"video_length"`
		Size           string `json:"size"`
		AspectRatio    string `json:"aspect_ratio"`
		ResolutionName string `json:"resolution"`
		Preset         string `json:"preset"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}
	seconds := raw.Seconds
	if seconds <= 0 {
		seconds = raw.Duration
	}
	if seconds <= 0 {
		seconds = raw.VideoLength
	}
	return &GrokVideoRequest{
		Model:          strings.TrimSpace(raw.Model),
		Prompt:         strings.TrimSpace(raw.Prompt),
		Seconds:        seconds,
		Size:           strings.TrimSpace(raw.Size),
		AspectRatio:    strings.TrimSpace(raw.AspectRatio),
		ResolutionName: strings.TrimSpace(raw.ResolutionName),
		Preset:         strings.TrimSpace(raw.Preset),
	}, nil
}

func flattenChatMessages(messages []struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}) string {
	parts := make([]string, 0, len(messages))
	for _, msg := range messages {
		text := messageContentText(msg.Content)
		if strings.TrimSpace(text) == "" {
			continue
		}
		role := strings.TrimSpace(msg.Role)
		if role != "" {
			parts = append(parts, role+": "+text)
		} else {
			parts = append(parts, text)
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

func messageContentText(content any) string {
	switch v := content.(type) {
	case string:
		return strings.TrimSpace(v)
	case []any:
		parts := make([]string, 0, len(v))
		for _, item := range v {
			part, _ := item.(map[string]any)
			if part == nil {
				continue
			}
			partType := strings.TrimSpace(fmt.Sprint(part["type"]))
			switch partType {
			case "text", "input_text":
				if text := strings.TrimSpace(fmt.Sprint(part["text"])); text != "" && text != "<nil>" {
					parts = append(parts, text)
				}
			}
		}
		return strings.TrimSpace(strings.Join(parts, "\n"))
	default:
		return ""
	}
}

func grokSSOFromAccount(account *Account) string {
	return firstCredential(account, "sso", "session_sso", "sso_token")
}

func firstCredential(account *Account, keys ...string) string {
	if account == nil {
		return ""
	}
	for _, key := range keys {
		if v := strings.TrimSpace(account.GetCredential(key)); v != "" {
			return v
		}
	}
	return ""
}

func buildGrokExtraCookies(account *Account) string {
	values := make([]string, 0, 3)
	if cookies := firstCredential(account, "extra_cookies", "cf_cookies", "cookies"); cookies != "" {
		values = append(values, strings.Trim(cookies, "; "))
	}
	if clearance := firstCredential(account, "cf_clearance"); clearance != "" && !strings.Contains(strings.Join(values, "; "), "cf_clearance=") {
		values = append(values, "cf_clearance="+clearance)
	}
	return strings.Join(values, "; ")
}

func aspectRatioFromSize(size string) string {
	normalized := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(size), " ", ""))
	if normalized == "" {
		return ""
	}
	switch normalized {
	case "1024x1024", "1536x1536", "1:1", "square":
		return "1:1"
	case "1024x1792", "720x1280", "9:16", "portrait":
		return "9:16"
	case "1792x1024", "1280x720", "16:9", "landscape":
		return "16:9"
	case "2:3", "3:2":
		return normalized
	default:
		parts := strings.Split(normalized, "x")
		if len(parts) != 2 {
			return ""
		}
		w, errW := strconv.Atoi(parts[0])
		h, errH := strconv.Atoi(parts[1])
		if errW != nil || errH != nil || w <= 0 || h <= 0 {
			return ""
		}
		switch {
		case w == h:
			return "1:1"
		case w > h:
			return "16:9"
		default:
			return "9:16"
		}
	}
}

func estimateTokens(text string) int {
	text = strings.TrimSpace(text)
	if text == "" {
		return 0
	}
	count := utf8.RuneCountInString(text)
	tokens := count / 4
	if tokens < 1 {
		tokens = 1
	}
	return tokens
}

func optionalUpstreamModel(requested, upstream string) string {
	if strings.TrimSpace(upstream) == "" || upstream == requested {
		return ""
	}
	return upstream
}
