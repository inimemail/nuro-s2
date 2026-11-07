package service

import (
	"compress/gzip"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/stretchr/testify/require"
)

type backupLifecycleReader struct {
	io.Reader
	closeErr   error
	closeCalls int
}

func (r *backupLifecycleReader) Close() error {
	r.closeCalls++
	return r.closeErr
}

type backupLifecycleDumper struct {
	reader io.ReadCloser
}

func (d *backupLifecycleDumper) Dump(context.Context) (io.ReadCloser, error) {
	return d.reader, nil
}

func (*backupLifecycleDumper) Restore(context.Context, io.Reader) error {
	return errors.New("restore is not used by this fixture")
}

func TestCreateCompressedBackupFileChecksDumpCompletion(t *testing.T) {
	readErr := errors.New("dump stream interrupted")
	exitErr := errors.New("pg_dump exited unsuccessfully")
	for _, tc := range []struct {
		name     string
		readErr  error
		closeErr error
		wantErr  error
	}{
		{name: "success"},
		{name: "process failure after output", closeErr: exitErr, wantErr: exitErr},
		{name: "read failure", readErr: readErr, wantErr: readErr},
		{name: "read failure takes precedence", readErr: readErr, closeErr: exitErr, wantErr: readErr},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tmp := t.TempDir()
			t.Setenv("TMPDIR", tmp)
			const dump = "-- PostgreSQL dump\nCREATE TABLE example (id int);\n"
			var source io.Reader = strings.NewReader(dump)
			if tc.readErr != nil {
				source = io.MultiReader(source, iotest.ErrReader(tc.readErr))
			}
			reader := &backupLifecycleReader{Reader: source, closeErr: tc.closeErr}
			svc := &BackupService{dumper: &backupLifecycleDumper{reader: reader}}

			path, size, err := svc.createCompressedBackupFile(context.Background())
			require.Equal(t, 1, reader.closeCalls, "dump process must be waited for exactly once")
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
				require.Empty(t, path)
				require.Zero(t, size)
				entries, readDirErr := os.ReadDir(tmp)
				require.NoError(t, readDirErr)
				require.Empty(t, entries, "failed dump must not leave an archive")
				return
			}
			require.NoError(t, err)
			info, err := os.Stat(path)
			require.NoError(t, err)
			require.Equal(t, info.Size(), size)
			require.Equal(t, os.FileMode(0600), info.Mode().Perm())
			f, err := os.Open(path)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, f.Close()) })
			zr, err := gzip.NewReader(f)
			require.NoError(t, err)
			body, err := io.ReadAll(zr)
			require.NoError(t, err)
			require.NoError(t, zr.Close())
			require.Equal(t, dump, string(body))
		})
	}
}
