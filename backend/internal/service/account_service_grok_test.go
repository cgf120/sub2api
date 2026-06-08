package service

import (
	"context"
	"strings"
	"testing"
)

type accountServiceRepoStub struct {
	AccountRepository

	account     *Account
	createCalls int
	updateCalls int
	bindCalls   int
}

func (s *accountServiceRepoStub) Create(_ context.Context, account *Account) error {
	s.createCalls++
	s.account = account
	if s.account.ID == 0 {
		s.account.ID = 1
	}
	return nil
}

func (s *accountServiceRepoStub) GetByID(_ context.Context, _ int64) (*Account, error) {
	return s.account, nil
}

func (s *accountServiceRepoStub) Update(_ context.Context, account *Account) error {
	s.updateCalls++
	s.account = account
	return nil
}

func (s *accountServiceRepoStub) BindGroups(_ context.Context, _ int64, _ []int64) error {
	s.bindCalls++
	return nil
}

type accountServiceGroupRepoStub struct {
	GroupRepository

	groups map[int64]*Group
}

func (s *accountServiceGroupRepoStub) GetByID(_ context.Context, id int64) (*Group, error) {
	return s.groups[id], nil
}

func TestAccountServiceCreateRejectsAPIKeyInGrokOAuthOnlyGroupBeforeWrite(t *testing.T) {
	accountRepo := &accountServiceRepoStub{}
	groupRepo := &accountServiceGroupRepoStub{groups: map[int64]*Group{
		7: {ID: 7, Name: "grok-group", Platform: PlatformGrok, RequireOAuthOnly: true},
	}}
	svc := NewAccountService(accountRepo, groupRepo)

	_, err := svc.Create(context.Background(), CreateAccountRequest{
		Name:        "api-key-account",
		Platform:    PlatformGrok,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"api_key": "sk-test"},
		GroupIDs:    []int64{7},
	})
	if err == nil || !strings.Contains(err.Error(), "仅允许 OAuth 账号") {
		t.Fatalf("Create() error=%v, want OAuth-only rejection", err)
	}
	if accountRepo.createCalls != 0 {
		t.Fatalf("Create() wrote account before validation, createCalls=%d", accountRepo.createCalls)
	}
	if accountRepo.bindCalls != 0 {
		t.Fatalf("Create() bound groups after validation failure, bindCalls=%d", accountRepo.bindCalls)
	}
}

func TestAccountServiceUpdateRejectsAPIKeyInGrokOAuthOnlyGroupBeforeWrite(t *testing.T) {
	accountRepo := &accountServiceRepoStub{account: &Account{
		ID:       1,
		Name:     "api-key-account",
		Platform: PlatformGrok,
		Type:     AccountTypeAPIKey,
		Status:   StatusActive,
	}}
	groupRepo := &accountServiceGroupRepoStub{groups: map[int64]*Group{
		7: {ID: 7, Name: "grok-group", Platform: PlatformGrok, RequireOAuthOnly: true},
	}}
	svc := NewAccountService(accountRepo, groupRepo)
	groupIDs := []int64{7}
	newName := "renamed"

	_, err := svc.Update(context.Background(), 1, UpdateAccountRequest{Name: &newName, GroupIDs: &groupIDs})
	if err == nil || !strings.Contains(err.Error(), "仅允许 OAuth 账号") {
		t.Fatalf("Update() error=%v, want OAuth-only rejection", err)
	}
	if accountRepo.updateCalls != 0 {
		t.Fatalf("Update() wrote account before validation, updateCalls=%d", accountRepo.updateCalls)
	}
	if accountRepo.bindCalls != 0 {
		t.Fatalf("Update() bound groups after validation failure, bindCalls=%d", accountRepo.bindCalls)
	}
	if accountRepo.account.Name != "api-key-account" {
		t.Fatalf("Update() mutated account before validation, name=%q", accountRepo.account.Name)
	}
}
