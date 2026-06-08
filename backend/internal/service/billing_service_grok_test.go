package service

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

func TestBillingServiceGrokTextFallbackPreventsZeroCost(t *testing.T) {
	svc := NewBillingService(&config.Config{}, nil)
	resolver := NewModelPricingResolver(nil, svc)

	cost, err := svc.CalculateCostUnified(CostInput{
		Ctx:   context.Background(),
		Model: "grok-4.20-fast",
		Tokens: UsageTokens{
			InputTokens:     1000,
			OutputTokens:    500,
			CacheReadTokens: 100,
		},
		RateMultiplier: 1.0,
		Resolver:       resolver,
	})
	if err != nil {
		t.Fatalf("CalculateCostUnified() error = %v", err)
	}

	want := 1000*grokTextInputPricePerToken +
		500*grokTextOutputPricePerToken +
		100*grokTextCacheReadPricePerToken
	if diff := cost.TotalCost - want; diff < -1e-12 || diff > 1e-12 {
		t.Fatalf("TotalCost = %.12f, want %.12f", cost.TotalCost, want)
	}
	if cost.TotalCost <= 0 {
		t.Fatalf("TotalCost = %.12f, want positive", cost.TotalCost)
	}
	if cost.BillingMode != string(BillingModeToken) {
		t.Fatalf("BillingMode = %q, want %q", cost.BillingMode, BillingModeToken)
	}
}

func TestBillingServiceGrokImageFallbackPrices(t *testing.T) {
	svc := NewBillingService(&config.Config{}, nil)

	lite := svc.CalculateImageCost("grok-imagine-image-lite", "1K", 1, nil, 1.0)
	if diff := lite.TotalCost - grokImageLitePricePerImage; diff < -1e-12 || diff > 1e-12 {
		t.Fatalf("lite TotalCost = %.12f, want %.12f", lite.TotalCost, grokImageLitePricePerImage)
	}

	pro := svc.CalculateImageCost("grok-imagine-image-pro", "2K", 1, nil, 1.0)
	if diff := pro.TotalCost - grokImagePro2KPricePerImage; diff < -1e-12 || diff > 1e-12 {
		t.Fatalf("pro TotalCost = %.12f, want %.12f", pro.TotalCost, grokImagePro2KPricePerImage)
	}
}

func TestBillingServiceGrokVideoFallbackPricesSeconds(t *testing.T) {
	svc := NewBillingService(&config.Config{}, nil)
	resolver := NewModelPricingResolver(nil, svc)

	cost, err := svc.CalculateCostUnified(CostInput{
		Ctx:            context.Background(),
		Model:          "grok-imagine-video",
		Tokens:         UsageTokens{OutputTokens: 6},
		RateMultiplier: 1.0,
		Resolver:       resolver,
	})
	if err != nil {
		t.Fatalf("CalculateCostUnified() error = %v", err)
	}

	want := 6 * grokVideoOutputPricePerSecond
	if diff := cost.TotalCost - want; diff < -1e-12 || diff > 1e-12 {
		t.Fatalf("TotalCost = %.12f, want %.12f", cost.TotalCost, want)
	}
	if cost.TotalCost <= 0 {
		t.Fatalf("TotalCost = %.12f, want positive", cost.TotalCost)
	}
}
