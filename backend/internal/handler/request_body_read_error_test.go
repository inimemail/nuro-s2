package handler

import (
	"context"
	"errors"
	"github.com/stretchr/testify/require"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

func TestRequestBodyFailureTimeoutBeforeCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest("POST", "/", nil).WithContext(ctx)
	cases := []struct {
		err  error
		want int
	}{
		{os.ErrDeadlineExceeded, 408},
		{context.DeadlineExceeded, 408},
		{context.Canceled, 499},
		{&http.MaxBytesError{Limit: 1024}, 413},
	}
	for _, tt := range cases {
		code, _ := requestBodyReadFailure(req, tt.err)
		require.Equal(t, tt.want, code)
	}
	code, _ := requestBodyReadFailure(httptest.NewRequest("POST", "/", nil), errors.New("read failed"))
	require.Equal(t, 400, code)
}
