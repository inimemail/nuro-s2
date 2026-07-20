package repository

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestSchedulerOutboxGroupChangesAreNeverDeduplicated(t *testing.T) {
	require.False(t, schedulerOutboxEventSupportsDedup(service.SchedulerOutboxEventGroupChanged))
	require.False(t, schedulerOutboxEventSupportsDedup(service.SchedulerOutboxEventAccountGroupsChanged))
	require.True(t, schedulerOutboxEventSupportsDedup(service.SchedulerOutboxEventAccountChanged))
	require.True(t, schedulerOutboxEventSupportsDedup(service.SchedulerOutboxEventFullRebuild))
}
