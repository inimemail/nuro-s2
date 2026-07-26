package repository

import (
	"context"
	"fmt"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func seedUserForAliasTest(t *testing.T, repo *userRepository, email string) {
	t.Helper()
	require.NoError(t, repo.Create(context.Background(), &service.User{
		Email: email, Username: email, PasswordHash: "hash",
		Role: service.RoleUser, Status: service.StatusActive,
	}))
}

func TestUserRepositoryExistsByEmailAliasCannotBeStarvedByNonGmailDotVariants(t *testing.T) {
	repo, _ := newUserEntRepo(t)
	ctx := context.Background()
	for mask := 1; mask <= emailAliasCandidateLimit+10; mask++ {
		local := "abcdefghij"
		position := 1 + (mask % (len(local) - 1))
		variant := local[:position] + "." + local[position:] + fmt.Sprintf("+%d", mask)
		seedUserForAliasTest(t, repo, variant+"@qq.com")
	}
	seedUserForAliasTest(t, repo, "abc.defghij+existing@qq.com")

	exists, err := repo.ExistsByEmailAlias(ctx, "abc.defghij+new@qq.com")
	require.NoError(t, err)
	require.True(t, exists)
}

func TestUserRepositoryExistsByEmailAlias(t *testing.T) {
	tests := []struct {
		name, stored, probe string
		want                bool
	}{
		{name: "gmail plus and dots", stored: "d.axis.2026@gmail.com", probe: "da.xis.2026+free@googlemail.com.", want: true},
		{name: "stored alias", stored: "someone+tag@gmail.com", probe: "some.one@gmail.com", want: true},
		{name: "non gmail plus", stored: "first.last@qq.com", probe: "first.last+tag@qq.com", want: true},
		{name: "non gmail dots significant", stored: "first.last@qq.com", probe: "firstlast@qq.com", want: false},
		{name: "underscore literal", stored: "user_x@qq.com", probe: "userax@qq.com", want: false},
		{name: "percent literal", stored: "a%b@qq.com", probe: "axxb@qq.com", want: false},
		{name: "leading plus stays distinct", stored: "+alice@gmail.com", probe: "+bob@gmail.com", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo, _ := newUserEntRepo(t)
			seedUserForAliasTest(t, repo, tt.stored)
			got, err := repo.ExistsByEmailAlias(context.Background(), tt.probe)
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestUserRepositoryCreateWithEmailAliasGuard(t *testing.T) {
	repo, _ := newUserEntRepo(t)
	ctx := context.Background()
	seedUserForAliasTest(t, repo, "d.axis.2026@gmail.com")

	err := repo.CreateWithEmailAliasGuard(ctx, &service.User{
		Email: "da.xis.2026+free@googlemail.com", Username: "alias",
		PasswordHash: "hash", Role: service.RoleUser, Status: service.StatusActive,
	})
	require.ErrorIs(t, err, service.ErrEmailExists)

	require.NoError(t, repo.Create(ctx, &service.User{
		Email: "daxis2026+support@gmail.com", Username: "admin-created",
		PasswordHash: "hash", Role: service.RoleUser, Status: service.StatusActive,
	}))
}
