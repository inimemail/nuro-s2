//go:build unit

package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAdminService_UpdateUser_AllowsRoleChangeWhenAdminRemains(t *testing.T) {
	repo := &userRepoStub{
		user:      &User{ID: 42, Email: "admin@example.com", Role: RoleAdmin, Status: StatusActive},
		listUsers: []User{{ID: 42, Role: RoleAdmin}, {ID: 43, Role: RoleAdmin}},
	}
	invalidator := &authCacheInvalidatorStub{}
	svc := &adminServiceImpl{
		userRepo:             repo,
		redeemCodeRepo:       &redeemRepoStub{},
		authCacheInvalidator: invalidator,
	}

	updated, err := svc.UpdateUser(context.Background(), 42, &UpdateUserInput{
		Role: RoleUser,
	})

	require.NoError(t, err)
	require.Equal(t, RoleUser, updated.Role)
	require.Equal(t, []int64{42}, invalidator.userIDs)
	require.Len(t, repo.updated, 1)
}

func TestAdminService_UpdateUser_RejectsLastAdminDemotion(t *testing.T) {
	repo := &userRepoStub{
		user:      &User{ID: 42, Email: "admin@example.com", Role: RoleAdmin, Status: StatusActive},
		listUsers: []User{{ID: 42, Role: RoleAdmin}},
	}
	svc := &adminServiceImpl{
		userRepo:       repo,
		redeemCodeRepo: &redeemRepoStub{},
	}

	_, err := svc.UpdateUser(context.Background(), 42, &UpdateUserInput{
		Role: RoleUser,
	})

	require.ErrorContains(t, err, "cannot demote the last admin user")
	require.Empty(t, repo.updated)
}

func TestAdminService_UpdateUser_RejectsSelfDemotion(t *testing.T) {
	repo := &userRepoStub{
		user:      &User{ID: 42, Email: "admin@example.com", Role: RoleAdmin, Status: StatusActive},
		listUsers: []User{{ID: 42, Role: RoleAdmin}, {ID: 43, Role: RoleAdmin}},
	}
	svc := &adminServiceImpl{
		userRepo:       repo,
		redeemCodeRepo: &redeemRepoStub{},
	}

	_, err := svc.UpdateUser(context.Background(), 42, &UpdateUserInput{
		Role:         RoleUser,
		ActorAdminID: 42,
	})

	require.ErrorContains(t, err, "cannot demote yourself from admin")
	require.Empty(t, repo.updated)
}

func TestAdminService_UpdateUser_RejectsInvalidRole(t *testing.T) {
	repo := &userRepoStub{user: &User{ID: 42, Email: "user@example.com", Role: RoleUser, Status: StatusActive}}
	svc := &adminServiceImpl{
		userRepo:       repo,
		redeemCodeRepo: &redeemRepoStub{},
	}

	_, err := svc.UpdateUser(context.Background(), 42, &UpdateUserInput{
		Role: "owner",
	})

	require.ErrorContains(t, err, "invalid role")
	require.Empty(t, repo.updated)
}
