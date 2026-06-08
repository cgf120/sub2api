package grok

import (
	"context"
	"strings"
)

type RateLimitSnapshot struct {
	ModelName            string         `json:"modelName,omitempty"`
	RequestKind          string         `json:"requestKind,omitempty"`
	RemainingQueries     *int           `json:"remainingQueries,omitempty"`
	TotalQueries         *int           `json:"totalQueries,omitempty"`
	WindowSizeSeconds    *int           `json:"windowSizeSeconds,omitempty"`
	WaitTimeSeconds      *int           `json:"waitTimeSeconds,omitempty"`
	HighEffortRateLimits map[string]any `json:"highEffortRateLimits,omitempty"`
	LowEffortRateLimits  map[string]any `json:"lowEffortRateLimits,omitempty"`
}

func (c *Client) FetchRateLimits(ctx context.Context, modelName, requestKind string) (*RateLimitSnapshot, error) {
	modelName = strings.TrimSpace(modelName)
	if modelName == "" {
		modelName = "fast"
	}
	requestKind = strings.TrimSpace(requestKind)
	payload := map[string]any{"modelName": modelName}
	if requestKind != "" {
		payload["requestKind"] = requestKind
	}
	var snapshot RateLimitSnapshot
	if err := c.postJSON(ctx, RateLimitsURL, BaseURL+"/", payload, &snapshot); err != nil {
		return nil, err
	}
	snapshot.ModelName = modelName
	snapshot.RequestKind = requestKind
	return &snapshot, nil
}
