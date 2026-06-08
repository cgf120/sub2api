package service

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/grok"
)

const (
	grokBudgetExtraKey = "grok_budget_policy"

	grokBudgetScopeImageLite   = "grok:image:lite"
	grokBudgetScopeImageNormal = "grok:image:normal"
	grokBudgetScopeImagePro    = "grok:image:pro"
	grokBudgetScopeVideo       = "grok:video"
)

type GrokBudgetCache interface {
	GetGrokBudgetUsage(ctx context.Context, accountID int64, scope string, window time.Duration) (int, error)
	ReserveGrokBudget(ctx context.Context, accountID int64, scope string, cost, limit int, window time.Duration) (*GrokBudgetReservation, error)
}

type GrokBudgetReservation struct {
	Allowed bool
	Used    int
	Limit   int
	Scope   string
	Cost    int
}

type grokBudgetPolicy struct {
	Scope  string
	Cost   int
	Limit  int
	Window time.Duration
}

func grokBudgetScopeForModel(model string) string {
	switch strings.TrimSpace(model) {
	case grok.ModelImageLite:
		return grokBudgetScopeImageLite
	case grok.ModelImagePro:
		return grokBudgetScopeImagePro
	case grok.ModelImage:
		return grokBudgetScopeImageNormal
	case grok.ModelImagineVideo:
		return grokBudgetScopeVideo
	default:
		return ""
	}
}

func grokBudgetScopeForAccountModel(account *Account, requestedModel string) string {
	model := strings.TrimSpace(requestedModel)
	if account != nil {
		if mapped, _ := account.ResolveMappedModel(model); strings.TrimSpace(mapped) != "" {
			model = mapped
		}
	}
	return grokBudgetScopeForModel(model)
}

func defaultGrokBudgetCost(scope string) int {
	switch scope {
	case grokBudgetScopeImageLite:
		return 2
	case grokBudgetScopeImageNormal:
		return 6
	case grokBudgetScopeImagePro:
		return 4
	case grokBudgetScopeVideo:
		return 1
	default:
		return 1
	}
}

func defaultGrokBudgetWindow(scope string) time.Duration {
	switch scope {
	case grokBudgetScopeVideo:
		return 24 * time.Hour
	case grokBudgetScopeImageLite, grokBudgetScopeImageNormal, grokBudgetScopeImagePro:
		return 2 * time.Hour
	default:
		return time.Hour
	}
}

func grokBudgetPolicyForAccount(account *Account, requestedModel string) (grokBudgetPolicy, bool) {
	scope := grokBudgetScopeForAccountModel(account, requestedModel)
	if scope == "" || account == nil || account.Extra == nil {
		return grokBudgetPolicy{}, false
	}
	rawPolicies, ok := account.Extra[grokBudgetExtraKey].(map[string]any)
	if !ok {
		return grokBudgetPolicy{}, false
	}
	rawPolicy, ok := grokRawBudgetPolicy(rawPolicies, scope)
	if !ok {
		return grokBudgetPolicy{}, false
	}

	limit := int(parseExtraFloat64(rawPolicy["limit"]))
	if limit <= 0 {
		return grokBudgetPolicy{}, false
	}
	cost := int(parseExtraFloat64(rawPolicy["cost_per_request"]))
	if cost <= 0 {
		cost = int(parseExtraFloat64(rawPolicy["cost"]))
	}
	if cost <= 0 {
		cost = defaultGrokBudgetCost(scope)
	}
	windowSeconds := int(parseExtraFloat64(rawPolicy["window_seconds"]))
	window := time.Duration(windowSeconds) * time.Second
	if window <= 0 {
		window = defaultGrokBudgetWindow(scope)
	}
	return grokBudgetPolicy{
		Scope:  scope,
		Cost:   cost,
		Limit:  limit,
		Window: window,
	}, true
}

func grokRawBudgetPolicy(policies map[string]any, scope string) (map[string]any, bool) {
	for _, key := range grokBudgetPolicyKeys(scope) {
		if raw, ok := policies[key]; ok {
			if policy, ok := raw.(map[string]any); ok {
				return policy, true
			}
		}
	}
	return nil, false
}

func grokBudgetPolicyKeys(scope string) []string {
	switch scope {
	case grokBudgetScopeImageLite:
		return []string{scope, "image_lite", "lite"}
	case grokBudgetScopeImageNormal:
		return []string{scope, "image_normal", "normal", "image"}
	case grokBudgetScopeImagePro:
		return []string{scope, "image_pro", "pro"}
	case grokBudgetScopeVideo:
		return []string{scope, "video"}
	default:
		return []string{scope}
	}
}

func (s *GrokGatewayService) isGrokBudgetAvailable(ctx context.Context, account *Account, requestedModel string) bool {
	policy, ok := grokBudgetPolicyForAccount(account, requestedModel)
	if !ok || s == nil || s.budgetCache == nil {
		return true
	}
	used, err := s.budgetCache.GetGrokBudgetUsage(ctx, account.ID, policy.Scope, policy.Window)
	if err != nil {
		slog.Warn("grok.budget.get_usage_failed",
			"account_id", account.ID,
			"scope", policy.Scope,
			"error", err,
		)
		return true
	}
	return used+policy.Cost <= policy.Limit
}

func (s *GrokGatewayService) reserveGrokBudget(ctx context.Context, account *Account, requestedModel string) (bool, string) {
	policy, ok := grokBudgetPolicyForAccount(account, requestedModel)
	if !ok || s == nil || s.budgetCache == nil {
		return true, ""
	}
	reservation, err := s.budgetCache.ReserveGrokBudget(ctx, account.ID, policy.Scope, policy.Cost, policy.Limit, policy.Window)
	if err != nil {
		slog.Warn("grok.budget.reserve_failed",
			"account_id", account.ID,
			"scope", policy.Scope,
			"error", err,
		)
		return true, ""
	}
	if reservation == nil || reservation.Allowed {
		return true, ""
	}
	return false, fmt.Sprintf("grok budget exceeded for %s: used=%d cost=%d limit=%d", policy.Scope, reservation.Used, policy.Cost, policy.Limit)
}

func (s *GrokGatewayService) ReserveBudgetForRequest(ctx context.Context, account *Account, requestedModel string) (bool, string) {
	return s.reserveGrokBudget(ctx, account, requestedModel)
}
