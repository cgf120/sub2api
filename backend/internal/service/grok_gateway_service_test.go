package service

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/grok"
)

func TestGrokAccountMaxConcurrency_DefaultMediaCaps(t *testing.T) {
	account := &Account{Concurrency: 10}

	tests := []struct {
		name  string
		model string
		want  int
	}{
		{name: "text caps at two", model: grok.ModelChatFast, want: 2},
		{name: "lite image caps at two", model: grok.ModelImageLite, want: 2},
		{name: "normal image caps at one", model: grok.ModelImage, want: 1},
		{name: "pro image caps at one", model: grok.ModelImagePro, want: 1},
		{name: "video caps at one", model: grok.ModelImagineVideo, want: 1},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := grokAccountMaxConcurrency(account, tc.model); got != tc.want {
				t.Fatalf("grokAccountMaxConcurrency()=%d, want %d", got, tc.want)
			}
		})
	}
}

func TestGrokAccountMaxConcurrency_LoadFactorStillCapped(t *testing.T) {
	loadFactor := 10
	account := &Account{Concurrency: 10, LoadFactor: &loadFactor}

	tests := []struct {
		name  string
		model string
		want  int
	}{
		{name: "text caps load factor", model: grok.ModelChatFast, want: 2},
		{name: "lite caps load factor", model: grok.ModelImageLite, want: 2},
		{name: "normal caps load factor", model: grok.ModelImage, want: 1},
		{name: "pro caps load factor", model: grok.ModelImagePro, want: 1},
		{name: "video caps load factor", model: grok.ModelImagineVideo, want: 1},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := grokAccountMaxConcurrency(account, tc.model); got != tc.want {
				t.Fatalf("grokAccountMaxConcurrency()=%d, want %d", got, tc.want)
			}
		})
	}
}

func TestGrokSelectAccountWithLoadAwareness_PrefersLowerLoad(t *testing.T) {
	svc := NewGrokGatewayService(
		stubOpenAIAccountRepo{accounts: []Account{
			grokTestAccount(1, 1),
			grokTestAccount(2, 1),
		}},
		NewConcurrencyService(stubConcurrencyCache{
			loadMap: map[int64]*AccountLoadInfo{
				1: {AccountID: 1, LoadRate: 90},
				2: {AccountID: 2, LoadRate: 10},
			},
		}),
		nil,
		nil,
	)

	selection, err := svc.SelectAccountWithLoadAwareness(context.Background(), nil, grok.ModelChatFast, nil)
	if err != nil {
		t.Fatalf("SelectAccountWithLoadAwareness error: %v", err)
	}
	if selection == nil || selection.Account == nil {
		t.Fatal("selection account is nil")
	}
	if selection.Account.ID != 2 {
		t.Fatalf("selected account=%d, want 2", selection.Account.ID)
	}
	if !selection.Acquired || selection.ReleaseFunc == nil {
		t.Fatal("selection should be acquired with release func")
	}
	selection.ReleaseFunc()
}

func TestGrokSelectAccountWithLoadAwareness_RoundRobinsEqualLoadTextCandidates(t *testing.T) {
	groupID := int64(1)
	svc := NewGrokGatewayService(
		stubOpenAIAccountRepo{accounts: []Account{
			grokTestAccount(1, 1),
			grokTestAccount(2, 1),
			grokTestAccount(3, 1),
		}},
		NewConcurrencyService(stubConcurrencyCache{
			loadMap: map[int64]*AccountLoadInfo{
				1: {AccountID: 1, LoadRate: 0},
				2: {AccountID: 2, LoadRate: 0},
				3: {AccountID: 3, LoadRate: 0},
			},
		}),
		nil,
		nil,
	)

	var got []int64
	for i := 0; i < 4; i++ {
		selection, err := svc.SelectAccountWithLoadAwareness(context.Background(), &groupID, grok.ModelChatFast, nil)
		if err != nil {
			t.Fatalf("SelectAccountWithLoadAwareness #%d error: %v", i+1, err)
		}
		if selection == nil || selection.Account == nil {
			t.Fatalf("SelectAccountWithLoadAwareness #%d returned nil account", i+1)
		}
		got = append(got, selection.Account.ID)
		selection.ReleaseFunc()
	}

	want := []int64{1, 2, 3, 1}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("selected accounts=%v, want %v", got, want)
	}
}

func TestGrokSelectAccountWithLoadAwareness_RoundRobinDoesNotCrossLoadBuckets(t *testing.T) {
	groupID := int64(1)
	svc := NewGrokGatewayService(
		stubOpenAIAccountRepo{accounts: []Account{
			grokTestAccount(1, 1),
			grokTestAccount(2, 1),
			grokTestAccount(3, 1),
		}},
		NewConcurrencyService(stubConcurrencyCache{
			loadMap: map[int64]*AccountLoadInfo{
				1: {AccountID: 1, LoadRate: 0},
				2: {AccountID: 2, LoadRate: 10},
				3: {AccountID: 3, LoadRate: 75},
			},
		}),
		nil,
		nil,
	)

	first, err := svc.SelectAccountWithLoadAwareness(context.Background(), &groupID, grok.ModelChatFast, nil)
	if err != nil {
		t.Fatalf("first selection error: %v", err)
	}
	first.ReleaseFunc()
	second, err := svc.SelectAccountWithLoadAwareness(context.Background(), &groupID, grok.ModelChatFast, nil)
	if err != nil {
		t.Fatalf("second selection error: %v", err)
	}
	second.ReleaseFunc()

	if first.Account.ID != 1 || second.Account.ID != 2 {
		t.Fatalf("selected accounts=(%d,%d), want low-bucket rotation (1,2)", first.Account.ID, second.Account.ID)
	}
}

func TestGrokSelectAccountWithLoadAwareness_SkipsBudgetExhaustedAccount(t *testing.T) {
	budget := &stubGrokBudgetCache{used: map[string]int{
		"1:grok:image:normal": 6,
	}}
	svc := NewGrokGatewayService(
		stubOpenAIAccountRepo{accounts: []Account{
			grokTestAccountWithBudget(1, 1),
			grokTestAccountWithBudget(2, 2),
		}},
		NewConcurrencyService(stubConcurrencyCache{
			loadMap: map[int64]*AccountLoadInfo{
				1: {AccountID: 1, LoadRate: 0},
				2: {AccountID: 2, LoadRate: 50},
			},
		}),
		budget,
		nil,
	)

	selection, err := svc.SelectAccountWithLoadAwareness(context.Background(), nil, grok.ModelImage, nil)
	if err != nil {
		t.Fatalf("SelectAccountWithLoadAwareness error: %v", err)
	}
	if selection == nil || selection.Account == nil {
		t.Fatal("selection account is nil")
	}
	if selection.Account.ID != 2 {
		t.Fatalf("selected account=%d, want 2", selection.Account.ID)
	}
	if got := budget.used["2:grok:image:normal"]; got != 6 {
		t.Fatalf("reserved budget=%d, want 6", got)
	}
	selection.ReleaseFunc()
}

func TestFormatGrokRateLimitSnapshot(t *testing.T) {
	remaining, total, window, wait := 8, 10, 7200, 0
	text := formatGrokRateLimitSnapshot(&grok.RateLimitSnapshot{
		ModelName:         "fast",
		RemainingQueries:  &remaining,
		TotalQueries:      &total,
		WindowSizeSeconds: &window,
		WaitTimeSeconds:   &wait,
	})

	want := "Grok 文本额度 model=fast remaining=8 total=10 window=7200s"
	if text != want {
		t.Fatalf("formatGrokRateLimitSnapshot()=%q, want %q", text, want)
	}
}

func grokTestAccount(id int64, priority int) Account {
	return Account{
		ID:          id,
		Platform:    PlatformGrok,
		Type:        AccountTypeOAuth,
		Status:      StatusActive,
		Schedulable: true,
		Concurrency: 10,
		Priority:    priority,
		Credentials: map[string]any{"sso": "test-sso"},
	}
}

func grokTestAccountWithBudget(id int64, priority int) Account {
	account := grokTestAccount(id, priority)
	account.Extra = map[string]any{
		"grok_budget_policy": map[string]any{
			"image_normal": map[string]any{
				"limit":            6,
				"window_seconds":   3600,
				"cost_per_request": 6,
			},
		},
	}
	return account
}

type stubGrokBudgetCache struct {
	used map[string]int
}

func (s *stubGrokBudgetCache) GetGrokBudgetUsage(_ context.Context, accountID int64, scope string, _ time.Duration) (int, error) {
	if s.used == nil {
		return 0, nil
	}
	return s.used[stubGrokBudgetKey(accountID, scope)], nil
}

func (s *stubGrokBudgetCache) ReserveGrokBudget(_ context.Context, accountID int64, scope string, cost, limit int, _ time.Duration) (*GrokBudgetReservation, error) {
	if s.used == nil {
		s.used = make(map[string]int)
	}
	key := stubGrokBudgetKey(accountID, scope)
	used := s.used[key]
	if used+cost > limit {
		return &GrokBudgetReservation{Allowed: false, Used: used, Limit: limit, Scope: scope, Cost: cost}, nil
	}
	used += cost
	s.used[key] = used
	return &GrokBudgetReservation{Allowed: true, Used: used, Limit: limit, Scope: scope, Cost: cost}, nil
}

func stubGrokBudgetKey(accountID int64, scope string) string {
	return fmt.Sprintf("%d:%s", accountID, scope)
}
