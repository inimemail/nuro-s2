package repository

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Re-execute only this helper test, without pg_dump or a database connection.
func TestBackupDumpChildProcess(t *testing.T) {
	mode := os.Getenv("NURO_BACKUP_DUMP_CHILD")
	if mode == "" {
		return
	}
	if _, err := io.WriteString(os.Stdout, "-- dump fixture\n"); err != nil {
		os.Exit(90)
	}
	switch mode {
	case "success":
		os.Exit(0)
	case "failure":
		os.Exit(7)
	case "wait-after-eof":
		if err := os.Stdout.Close(); err != nil {
			os.Exit(91)
		}
		time.Sleep(30 * time.Second)
		os.Exit(0)
	default:
		os.Exit(92)
	}
}

func TestCmdReadCloserReportsProcessCompletion(t *testing.T) {
	for _, mode := range []string{"success", "failure", "wait-after-eof"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			executable, err := os.Executable()
			require.NoError(t, err)
			cmd := exec.CommandContext(ctx, executable, "-test.run=^TestBackupDumpChildProcess$")
			cmd.Env = append(os.Environ(), "NURO_BACKUP_DUMP_CHILD="+mode)
			stdout, err := cmd.StdoutPipe()
			require.NoError(t, err)
			require.NoError(t, cmd.Start())
			reader := &cmdReadCloser{ReadCloser: stdout, cmd: cmd}
			closeReader := sync.OnceValue(reader.Close)
			t.Cleanup(func() {
				cancel()
				// Failure cases intentionally return a non-nil process exit error.
				_ = closeReader()
			})

			body, err := io.ReadAll(reader)
			require.NoError(t, err, "stdout EOF itself does not report the process exit status")
			require.Contains(t, string(body), "-- dump fixture\n")
			require.NoError(t, ctx.Err(), "fixture must produce EOF before the deadline")
			if mode == "wait-after-eof" {
				cancel()
			}
			err = closeReader()
			if mode == "success" {
				require.NoError(t, err)
			} else {
				var exitErr *exec.ExitError
				require.True(t, errors.As(err, &exitErr), "Close must preserve the process exit error: %v", err)
				if mode == "failure" {
					require.Equal(t, 7, exitErr.ExitCode())
				}
			}
			require.NotNil(t, cmd.ProcessState, "Close must reap the child process")
		})
	}
}
