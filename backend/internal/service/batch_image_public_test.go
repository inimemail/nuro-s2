package service

import (
	"context"
	"io"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

type batchImageGroupRepoStub struct {
	group *Group
	err   error
}

func TestBatchImageProviderRegistryHonorsVertexEnabled(t *testing.T) {
	disabled := NewBatchImageProviderRegistryFromConfig(&config.Config{})
	if _, ok := disabled.Get(BatchImageProviderVertex); ok {
		t.Fatal("vertex provider should not be registered when batch_image.vertex_enabled is false")
	}
	if _, ok := disabled.Get(BatchImageProviderGeminiAPI); !ok {
		t.Fatal("gemini_api provider should remain registered")
	}

	enabled := NewBatchImageProviderRegistryFromConfig(&config.Config{
		BatchImage: config.BatchImageConfig{VertexEnabled: true},
	})
	if _, ok := enabled.Get(BatchImageProviderVertex); !ok {
		t.Fatal("vertex provider should be registered when batch_image.vertex_enabled is true")
	}
}

func (r batchImageGroupRepoStub) GetByIDLite(context.Context, int64) (*Group, error) {
	return r.group, r.err
}

type batchImageAccountRepoStub struct {
	accounts []Account
}

func (r batchImageAccountRepoStub) GetByID(context.Context, int64) (*Account, error) {
	return nil, ErrAccountNotFound
}

func (r batchImageAccountRepoStub) ListSchedulableByPlatform(context.Context, string) ([]Account, error) {
	return append([]Account(nil), r.accounts...), nil
}

func (r batchImageAccountRepoStub) ListSchedulableByGroupIDAndPlatform(context.Context, int64, string) ([]Account, error) {
	return append([]Account(nil), r.accounts...), nil
}

type batchImageProviderStub struct {
	name string
}

func (p batchImageProviderStub) Name() string { return p.name }

func (p batchImageProviderStub) SupportsAccount(*Account) bool { return true }

func (p batchImageProviderStub) Submit(context.Context, *BatchImageJob, *Account, BatchImageInput) (*BatchProviderJob, error) {
	return nil, ErrBatchImageProviderSubmitFailed
}

func (p batchImageProviderStub) Get(context.Context, *BatchImageJob, *Account) (*BatchProviderStatus, error) {
	return nil, ErrBatchImageProviderSubmitFailed
}

func (p batchImageProviderStub) Cancel(context.Context, *BatchImageJob, *Account) error {
	return ErrBatchImageCancelFailed
}

func (p batchImageProviderStub) OpenResult(context.Context, *BatchImageJob, *Account) (io.ReadCloser, string, error) {
	return nil, "", ErrBatchImageResultMissing
}

func (p batchImageProviderStub) Cleanup(context.Context, *BatchImageJob, *Account, CleanupTarget) error {
	return nil
}

func TestBatchImageEnsureGroupAllowsBatchImageRequiresGeminiGroup(t *testing.T) {
	groupID := int64(10)
	tests := []struct {
		name    string
		groupID *int64
		group   *Group
		wantErr error
	}{
		{
			name:    "missing group id",
			groupID: nil,
			wantErr: ErrBatchImageGroupDisabled,
		},
		{
			name:    "zero group id",
			groupID: func() *int64 { v := int64(0); return &v }(),
			wantErr: ErrBatchImageGroupDisabled,
		},
		{
			name:    "openai group rejected",
			groupID: &groupID,
			group: &Group{
				Platform:                  PlatformOpenAI,
				AllowBatchImageGeneration: true,
			},
			wantErr: ErrBatchImageGroupDisabled,
		},
		{
			name:    "gemini group with disabled gate rejected",
			groupID: &groupID,
			group: &Group{
				Platform:                  PlatformGemini,
				AllowBatchImageGeneration: false,
			},
			wantErr: ErrBatchImageGroupDisabled,
		},
		{
			name:    "gemini group with gate enabled allowed",
			groupID: &groupID,
			group: &Group{
				Platform:                  PlatformGemini,
				AllowBatchImageGeneration: true,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := &BatchImagePublicService{
				GroupRepo: batchImageGroupRepoStub{group: tt.group},
			}

			err := svc.ensureGroupAllowsBatchImage(context.Background(), tt.groupID)
			if tt.wantErr == nil {
				if err != nil {
					t.Fatalf("ensureGroupAllowsBatchImage() error = %v, want nil", err)
				}
				return
			}
			if err != tt.wantErr {
				t.Fatalf("ensureGroupAllowsBatchImage() error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestBatchImageSelectProviderAndAccountUsesLowerNumericPriority(t *testing.T) {
	providerName := "test_provider"
	svc := &BatchImagePublicService{
		AccountRepo: batchImageAccountRepoStub{accounts: []Account{
			{ID: 2, Platform: PlatformGemini, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Priority: 50, Credentials: map[string]any{"api_key": "key-2"}},
			{ID: 1, Platform: PlatformGemini, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Priority: 10, Credentials: map[string]any{"api_key": "key-1"}},
		}},
		ProviderRegistry: NewBatchImageProviderRegistry(batchImageProviderStub{name: providerName}),
	}

	_, account, err := svc.selectProviderAndAccount(context.Background(), BatchImageOwner{}, providerName, "gemini-2.5-flash-image")
	if err != nil {
		t.Fatalf("selectProviderAndAccount() error = %v", err)
	}
	if account == nil || account.ID != 1 {
		t.Fatalf("selected account = %#v, want ID 1", account)
	}
}
