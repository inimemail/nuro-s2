package service

import (
	"context"
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/pkg/adminaccess"
	"github.com/stretchr/testify/require"
)

func TestScopedAdminProxySelectionFailsClosed(t *testing.T) {
	for _, mode := range []string{"random", "least_used"} {
		for _, failure := range []string{"query", "rows", "empty", "scan"} {
			t.Run(mode+"/"+failure, func(t *testing.T) {
				db, mock, err := sqlmock.New()
				require.NoError(t, err)
				t.Cleanup(func() { _ = db.Close() })
				mock.ExpectQuery("SELECT id FROM proxies").
					WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(11).AddRow(12)).RowsWillBeClosed()
				selection := mock.ExpectQuery("SELECT .* FROM unnest")
				switch failure {
				case "query":
					selection.WillReturnError(errors.New("fixture query failure"))
				case "rows":
					selection.WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(12).
						RowError(0, errors.New("fixture row failure"))).RowsWillBeClosed()
				case "empty":
					selection.WillReturnRows(sqlmock.NewRows([]string{"id"})).RowsWillBeClosed()
				case "scan":
					selection.WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("invalid")).RowsWillBeClosed()
				}
				policy := adminaccess.Policy{Rules: []adminaccess.Rule{{Permissions: []string{"proxies.read"}, ProxyIDs: []int64{11, 12}}}}
				proxy, changed, err := scopedChooseProxy(context.Background(), db, policy, true, mode, nil)
				require.Error(t, err)
				require.Nil(t, proxy, "failed selection must not silently choose the first candidate")
				require.False(t, changed)
				require.NoError(t, mock.ExpectationsWereMet())
			})
		}
	}
}
