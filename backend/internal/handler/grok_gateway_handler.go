package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	grokpkg "github.com/Wei-Shaw/sub2api/internal/pkg/grok"
	pkghttputil "github.com/Wei-Shaw/sub2api/internal/pkg/httputil"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ip"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

type GrokGatewayHandler struct {
	grokService              *service.GrokGatewayService
	usageRecorder            *service.OpenAIGatewayService
	billingCacheService      *service.BillingCacheService
	apiKeyService            *service.APIKeyService
	usageRecordWorkerPool    *service.UsageRecordWorkerPool
	contentModerationService *service.ContentModerationService
	concurrencyHelper        *ConcurrencyHelper
	cfg                      *config.Config
}

func NewGrokGatewayHandler(
	grokService *service.GrokGatewayService,
	usageRecorder *service.OpenAIGatewayService,
	concurrencyService *service.ConcurrencyService,
	billingCacheService *service.BillingCacheService,
	apiKeyService *service.APIKeyService,
	usageRecordWorkerPool *service.UsageRecordWorkerPool,
	contentModerationService *service.ContentModerationService,
	cfg *config.Config,
) *GrokGatewayHandler {
	pingInterval := time.Duration(0)
	if cfg != nil {
		pingInterval = time.Duration(cfg.Concurrency.PingInterval) * time.Second
	}
	return &GrokGatewayHandler{
		grokService:              grokService,
		usageRecorder:            usageRecorder,
		billingCacheService:      billingCacheService,
		apiKeyService:            apiKeyService,
		usageRecordWorkerPool:    usageRecordWorkerPool,
		contentModerationService: contentModerationService,
		concurrencyHelper:        NewConcurrencyHelper(concurrencyService, SSEPingFormatComment, pingInterval),
		cfg:                      cfg,
	}
}

func (h *GrokGatewayHandler) ChatCompletions(c *gin.Context) {
	start := time.Now()
	apiKey, subject, body, ok := h.readAuthenticatedBody(c, "handler.grok_gateway.chat_completions")
	if !ok {
		return
	}
	req, err := service.ParseGrokChatCompletionRequest(body)
	if err != nil {
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "Failed to parse request body")
		return
	}
	if req.Model == "" {
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "model is required")
		return
	}
	if grokpkg.IsMediaModel(req.Model) {
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", fmt.Sprintf("model %s is not supported on /v1/chat/completions; use /v1/images/generations or /v1/videos instead", req.Model))
		return
	}
	if req.Prompt == "" {
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "messages content is required")
		return
	}
	reqLog := requestLogger(c, "handler.grok_gateway.chat_completions",
		zap.Int64("user_id", subject.UserID),
		zap.Int64("api_key_id", apiKey.ID),
		zap.Any("group_id", apiKey.GroupID),
		zap.String("model", req.Model),
		zap.Bool("stream", req.Stream),
	)
	setOpsRequestContext(c, req.Model, req.Stream)
	setOpsEndpointContext(c, "", int16(service.RequestTypeFromLegacy(req.Stream, false)))
	if decision := h.checkContentModeration(c, reqLog, apiKey, subject, service.ContentModerationProtocolOpenAIChat, req.Model, body); decision != nil && decision.Blocked {
		h.errorResponse(c, contentModerationStatus(decision), contentModerationErrorCode(decision), decision.Message)
		return
	}
	subscription, _ := middleware2.GetSubscriptionFromContext(c)
	service.SetOpsLatencyMs(c, service.OpsAuthLatencyMsKey, time.Since(start).Milliseconds())
	streamStarted := false
	account, userRelease, accountRelease, ok := h.prepareAccount(c, reqLog, apiKey, subject, subscription, req.Model, req.Stream, &streamStarted)
	if !ok {
		return
	}
	if userRelease != nil {
		defer userRelease()
	}
	if accountRelease != nil {
		defer accountRelease()
	}
	setOpsSelectedAccount(c, account.ID, account.Platform)
	forwardStart := time.Now()

	var result *service.OpenAIForwardResult
	var text string
	if req.Stream {
		result, text, err = h.streamChat(c, account, req, &streamStarted)
	} else {
		result, text, err = h.grokService.Chat(c.Request.Context(), account, req, nil)
	}
	service.SetOpsLatencyMs(c, service.OpsResponseLatencyMsKey, time.Since(forwardStart).Milliseconds())
	if err != nil {
		reqLog.Warn("grok.chat.forward_failed", zap.Int64("account_id", account.ID), zap.Error(err))
		h.grokService.MarkUpstreamFailure(c.Request.Context(), req.Model, account, err)
		h.streamingAwareError(c, http.StatusBadGateway, "upstream_error", err.Error(), streamStarted)
		return
	}
	if !req.Stream {
		h.writeChatCompletion(c, result, text)
	}
	h.recordUsage(c, reqLog, apiKey, subject, subscription, account, result, body, "handler.grok_gateway.chat_completions")
}

func (h *GrokGatewayHandler) Images(c *gin.Context) {
	start := time.Now()
	apiKey, subject, body, ok := h.readAuthenticatedBody(c, "handler.grok_gateway.images")
	if !ok {
		return
	}
	req, err := service.ParseGrokImagesRequest(body)
	if err != nil {
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if req.Model == "" {
		req.Model = "grok-imagine-image"
	}
	if req.Prompt == "" {
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "prompt is required")
		return
	}
	reqLog := requestLogger(c, "handler.grok_gateway.images",
		zap.Int64("user_id", subject.UserID),
		zap.Int64("api_key_id", apiKey.ID),
		zap.Any("group_id", apiKey.GroupID),
		zap.String("model", req.Model),
	)
	if !service.GroupAllowsImageGeneration(apiKey.Group) {
		h.errorResponse(c, http.StatusForbidden, "permission_error", service.ImageGenerationPermissionMessage())
		return
	}
	setOpsRequestContext(c, req.Model, false)
	setOpsEndpointContext(c, "", int16(service.RequestTypeFromLegacy(false, false)))
	if decision := h.checkContentModeration(c, reqLog, apiKey, subject, service.ContentModerationProtocolOpenAIImages, req.Model, body); decision != nil && decision.Blocked {
		h.errorResponse(c, contentModerationStatus(decision), contentModerationErrorCode(decision), decision.Message)
		return
	}
	subscription, _ := middleware2.GetSubscriptionFromContext(c)
	service.SetOpsLatencyMs(c, service.OpsAuthLatencyMsKey, time.Since(start).Milliseconds())
	streamStarted := false
	account, userRelease, accountRelease, ok := h.prepareAccount(c, reqLog, apiKey, subject, subscription, req.Model, false, &streamStarted)
	if !ok {
		return
	}
	if userRelease != nil {
		defer userRelease()
	}
	if accountRelease != nil {
		defer accountRelease()
	}
	setOpsSelectedAccount(c, account.ID, account.Platform)
	forwardStart := time.Now()
	result, images, err := h.grokService.Images(c.Request.Context(), account, req)
	service.SetOpsLatencyMs(c, service.OpsResponseLatencyMsKey, time.Since(forwardStart).Milliseconds())
	if err != nil {
		reqLog.Warn("grok.images.forward_failed", zap.Int64("account_id", account.ID), zap.Error(err))
		h.grokService.MarkUpstreamFailure(c.Request.Context(), req.Model, account, err)
		h.errorResponse(c, http.StatusBadGateway, "upstream_error", err.Error())
		return
	}
	data := make([]gin.H, 0, len(images))
	for _, image := range images {
		data = append(data, gin.H{"url": image.URL})
	}
	c.JSON(http.StatusOK, gin.H{
		"created": time.Now().Unix(),
		"data":    data,
	})
	h.recordUsage(c, reqLog, apiKey, subject, subscription, account, result, body, "handler.grok_gateway.images")
}

func (h *GrokGatewayHandler) Videos(c *gin.Context) {
	start := time.Now()
	apiKey, subject, body, ok := h.readAuthenticatedBody(c, "handler.grok_gateway.videos")
	if !ok {
		return
	}
	req, err := service.ParseGrokVideoRequest(body)
	if err != nil {
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "Failed to parse request body")
		return
	}
	if req.Model == "" {
		req.Model = "grok-imagine-video"
	}
	if req.Prompt == "" {
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "prompt is required")
		return
	}
	reqLog := requestLogger(c, "handler.grok_gateway.videos",
		zap.Int64("user_id", subject.UserID),
		zap.Int64("api_key_id", apiKey.ID),
		zap.Any("group_id", apiKey.GroupID),
		zap.String("model", req.Model),
	)
	setOpsRequestContext(c, req.Model, false)
	setOpsEndpointContext(c, "", int16(service.RequestTypeFromLegacy(false, false)))
	if decision := h.checkContentModeration(c, reqLog, apiKey, subject, service.ContentModerationProtocolOpenAIChat, req.Model, body); decision != nil && decision.Blocked {
		h.errorResponse(c, contentModerationStatus(decision), contentModerationErrorCode(decision), decision.Message)
		return
	}
	subscription, _ := middleware2.GetSubscriptionFromContext(c)
	service.SetOpsLatencyMs(c, service.OpsAuthLatencyMsKey, time.Since(start).Milliseconds())
	streamStarted := false
	account, userRelease, accountRelease, ok := h.prepareAccount(c, reqLog, apiKey, subject, subscription, req.Model, false, &streamStarted)
	if !ok {
		return
	}
	if userRelease != nil {
		defer userRelease()
	}
	if accountRelease != nil {
		defer accountRelease()
	}
	setOpsSelectedAccount(c, account.ID, account.Platform)
	forwardStart := time.Now()
	result, video, err := h.grokService.Video(c.Request.Context(), account, req, nil)
	service.SetOpsLatencyMs(c, service.OpsResponseLatencyMsKey, time.Since(forwardStart).Milliseconds())
	if err != nil {
		reqLog.Warn("grok.videos.forward_failed", zap.Int64("account_id", account.ID), zap.Error(err))
		h.grokService.MarkUpstreamFailure(c.Request.Context(), req.Model, account, err)
		h.errorResponse(c, http.StatusBadGateway, "upstream_error", err.Error())
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"id":            video.PostID,
		"object":        "video",
		"created":       time.Now().Unix(),
		"model":         req.Model,
		"status":        "succeeded",
		"url":           video.PublicURL,
		"public_url":    video.PublicURL,
		"download_url":  video.DownloadURL,
		"private_url":   video.PrivateURL,
		"share_url":     video.ShareURL,
		"thumbnail_url": video.ThumbnailURL,
	})
	h.recordUsage(c, reqLog, apiKey, subject, subscription, account, result, body, "handler.grok_gateway.videos")
}

func (h *GrokGatewayHandler) readAuthenticatedBody(c *gin.Context, component string) (*service.APIKey, middleware2.AuthSubject, []byte, bool) {
	apiKey, ok := middleware2.GetAPIKeyFromContext(c)
	if !ok {
		h.errorResponse(c, http.StatusUnauthorized, "authentication_error", "Invalid API key")
		return nil, middleware2.AuthSubject{}, nil, false
	}
	subject, ok := middleware2.GetAuthSubjectFromContext(c)
	if !ok {
		h.errorResponse(c, http.StatusInternalServerError, "api_error", "User context not found")
		return nil, middleware2.AuthSubject{}, nil, false
	}
	body, err := pkghttputil.ReadRequestBodyWithPrealloc(c.Request)
	if err != nil {
		if maxErr, ok := extractMaxBytesError(err); ok {
			h.errorResponse(c, http.StatusRequestEntityTooLarge, "invalid_request_error", buildBodyTooLargeMessage(maxErr.Limit))
			return nil, middleware2.AuthSubject{}, nil, false
		}
		requestLogger(c, component).Warn("grok.read_body_failed", zap.Error(err))
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "Failed to read request body")
		return nil, middleware2.AuthSubject{}, nil, false
	}
	if len(body) == 0 {
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", "Request body is empty")
		return nil, middleware2.AuthSubject{}, nil, false
	}
	return apiKey, subject, body, true
}

func (h *GrokGatewayHandler) prepareAccount(
	c *gin.Context,
	reqLog *zap.Logger,
	apiKey *service.APIKey,
	subject middleware2.AuthSubject,
	subscription *service.UserSubscription,
	model string,
	stream bool,
	streamStarted *bool,
) (*service.Account, func(), func(), bool) {
	if h.billingCacheService != nil {
		if err := h.billingCacheService.CheckBillingEligibility(c.Request.Context(), apiKey.User, apiKey, apiKey.Group, subscription, service.QuotaPlatform(c.Request.Context(), apiKey)); err != nil {
			reqLog.Info("grok.billing_eligibility_check_failed", zap.Error(err))
			status, code, message, retryAfter := billingErrorDetails(err)
			if retryAfter > 0 {
				c.Header("Retry-After", strconv.Itoa(retryAfter))
			}
			h.streamingAwareError(c, status, code, message, streamStarted != nil && *streamStarted)
			return nil, nil, nil, false
		}
	}
	userRelease, err := h.concurrencyHelper.AcquireUserSlotWithWait(c, subject.UserID, subject.Concurrency, stream, streamStarted)
	if err != nil {
		h.streamingAwareError(c, http.StatusTooManyRequests, "rate_limit_error", err.Error(), streamStarted != nil && *streamStarted)
		return nil, nil, nil, false
	}
	selection, err := h.grokService.SelectAccountWithLoadAwareness(c.Request.Context(), apiKey.GroupID, model, nil)
	if err != nil {
		if userRelease != nil {
			userRelease()
		}
		markOpsRoutingCapacityLimitedIfNoAvailable(c, err)
		h.streamingAwareError(c, http.StatusServiceUnavailable, "api_error", "No available Grok accounts", streamStarted != nil && *streamStarted)
		return nil, nil, nil, false
	}
	if selection == nil || selection.Account == nil {
		if userRelease != nil {
			userRelease()
		}
		h.streamingAwareError(c, http.StatusServiceUnavailable, "api_error", "No available Grok accounts", streamStarted != nil && *streamStarted)
		return nil, nil, nil, false
	}
	account := selection.Account
	accountRelease := selection.ReleaseFunc
	if !selection.Acquired {
		waitPlan := selection.WaitPlan
		if waitPlan == nil {
			if userRelease != nil {
				userRelease()
			}
			h.streamingAwareError(c, http.StatusTooManyRequests, "rate_limit_error", "account concurrency limit reached", streamStarted != nil && *streamStarted)
			return nil, nil, nil, false
		}
		timeout := waitPlan.Timeout
		if timeout <= 0 {
			timeout = 30 * time.Second
		}
		accountRelease, err = h.concurrencyHelper.AcquireAccountSlotWithWaitTimeout(c, waitPlan.AccountID, waitPlan.MaxConcurrency, timeout, stream, streamStarted)
		if err != nil {
			if userRelease != nil {
				userRelease()
			}
			h.streamingAwareError(c, http.StatusTooManyRequests, "rate_limit_error", err.Error(), streamStarted != nil && *streamStarted)
			return nil, nil, nil, false
		}
		if ok, reason := h.grokService.ReserveBudgetForRequest(c.Request.Context(), account, model); !ok {
			if accountRelease != nil {
				accountRelease()
			}
			if userRelease != nil {
				userRelease()
			}
			if reason == "" {
				reason = "grok budget exceeded"
			}
			h.streamingAwareError(c, http.StatusTooManyRequests, "rate_limit_error", reason, streamStarted != nil && *streamStarted)
			return nil, nil, nil, false
		}
	}
	return account, userRelease, accountRelease, true
}

func (h *GrokGatewayHandler) streamChat(c *gin.Context, account *service.Account, req *service.GrokChatCompletionRequest, streamStarted *bool) (*service.OpenAIForwardResult, string, error) {
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")
	*streamStarted = true
	flusher, _ := c.Writer.(http.Flusher)
	created := time.Now().Unix()
	responseID := "chatcmpl-" + fmt.Sprintf("%d", created)
	writeChatStreamChunk(c, responseID, req.Model, created, gin.H{"role": "assistant"}, nil)
	if flusher != nil {
		flusher.Flush()
	}
	result, text, err := h.grokService.Chat(c.Request.Context(), account, req, func(token string) error {
		writeChatStreamChunk(c, responseID, req.Model, created, gin.H{"content": token}, nil)
		if flusher != nil {
			flusher.Flush()
		}
		return nil
	})
	if err != nil {
		return result, text, err
	}
	writeChatStreamChunk(c, responseID, req.Model, created, gin.H{}, ptrString("stop"))
	_, _ = fmt.Fprint(c.Writer, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
	return result, text, nil
}

func (h *GrokGatewayHandler) writeChatCompletion(c *gin.Context, result *service.OpenAIForwardResult, text string) {
	model := ""
	responseID := "chatcmpl-" + fmt.Sprintf("%d", time.Now().UnixNano())
	usage := gin.H{"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0}
	if result != nil {
		model = result.Model
		if result.ResponseID != "" {
			responseID = result.ResponseID
		}
		promptTokens := result.Usage.InputTokens
		completionTokens := result.Usage.OutputTokens
		usage = gin.H{
			"prompt_tokens":     promptTokens,
			"completion_tokens": completionTokens,
			"total_tokens":      promptTokens + completionTokens,
		}
	}
	c.JSON(http.StatusOK, gin.H{
		"id":      responseID,
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []gin.H{
			{
				"index": 0,
				"message": gin.H{
					"role":    "assistant",
					"content": text,
				},
				"finish_reason": "stop",
			},
		},
		"usage": usage,
	})
}

func writeChatStreamChunk(c *gin.Context, responseID, model string, created int64, delta gin.H, finishReason *string) {
	chunk := gin.H{
		"id":      responseID,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   model,
		"choices": []gin.H{
			{
				"index":         0,
				"delta":         delta,
				"finish_reason": finishReason,
			},
		},
	}
	data, _ := json.Marshal(chunk)
	_, _ = fmt.Fprintf(c.Writer, "data: %s\n\n", data)
}

func (h *GrokGatewayHandler) recordUsage(
	c *gin.Context,
	reqLog *zap.Logger,
	apiKey *service.APIKey,
	subject middleware2.AuthSubject,
	subscription *service.UserSubscription,
	account *service.Account,
	result *service.OpenAIForwardResult,
	body []byte,
	component string,
) {
	if h.usageRecorder == nil || result == nil || apiKey == nil || account == nil {
		return
	}
	userAgent := c.GetHeader("User-Agent")
	clientIP := ip.GetClientIP(c)
	requestPayloadHash := service.HashUsageRequestPayload(body)
	inboundEndpoint := GetInboundEndpoint(c)
	upstreamEndpoint := GetUpstreamEndpoint(c, account.Platform)
	h.submitUsageRecordTask(c.Request.Context(), result, func(ctx context.Context) {
		if err := h.usageRecorder.RecordUsage(ctx, &service.OpenAIRecordUsageInput{
			Result:             result,
			APIKey:             apiKey,
			User:               apiKey.User,
			Account:            account,
			Subscription:       subscription,
			InboundEndpoint:    inboundEndpoint,
			UpstreamEndpoint:   upstreamEndpoint,
			UserAgent:          userAgent,
			IPAddress:          clientIP,
			RequestPayloadHash: requestPayloadHash,
			APIKeyService:      h.apiKeyService,
		}); err != nil {
			logger.L().With(
				zap.String("component", component),
				zap.Int64("user_id", subject.UserID),
				zap.Int64("api_key_id", apiKey.ID),
				zap.Any("group_id", apiKey.GroupID),
				zap.String("model", result.Model),
				zap.Int64("account_id", account.ID),
			).Error("grok.record_usage_failed", zap.Error(err))
		}
	})
	if reqLog != nil {
		reqLog.Debug("grok.request_completed", zap.Int64("account_id", account.ID))
	}
}

func (h *GrokGatewayHandler) submitUsageRecordTask(parent context.Context, result *service.OpenAIForwardResult, task service.UsageRecordTask) {
	if task == nil {
		return
	}
	task = wrapUsageRecordTaskContext(parent, task)
	if h.usageRecordWorkerPool != nil {
		if result != nil && result.ImageCount > 0 {
			if mode := h.usageRecordWorkerPool.Submit(task); mode != service.UsageRecordSubmitModeDropped {
				return
			}
		} else {
			h.usageRecordWorkerPool.Submit(task)
			return
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	task(ctx)
}

func (h *GrokGatewayHandler) checkContentModeration(c *gin.Context, reqLog *zap.Logger, apiKey *service.APIKey, subject middleware2.AuthSubject, protocol string, model string, body []byte) *service.ContentModerationDecision {
	if h == nil || h.contentModerationService == nil {
		return nil
	}
	return runContentModeration(c, reqLog, h.contentModerationService, apiKey, subject, protocol, model, body)
}

func (h *GrokGatewayHandler) errorResponse(c *gin.Context, status int, errType, message string) {
	c.JSON(status, gin.H{
		"error": gin.H{
			"type":    errType,
			"message": message,
		},
	})
}

func (h *GrokGatewayHandler) streamingAwareError(c *gin.Context, status int, errType, message string, streamStarted bool) {
	if streamStarted || c.Writer.Written() {
		c.Header("Content-Type", "text/event-stream")
		payload, _ := json.Marshal(gin.H{"error": gin.H{"type": errType, "message": message}})
		_, _ = fmt.Fprintf(c.Writer, "data: %s\n\n", payload)
		_, _ = fmt.Fprint(c.Writer, "data: [DONE]\n\n")
		if flusher, ok := c.Writer.(http.Flusher); ok {
			flusher.Flush()
		}
		return
	}
	h.errorResponse(c, status, errType, message)
}

func ptrString(s string) *string {
	return &s
}
