package service

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
)

type pricingTempFile interface {
	io.Writer
	Sync() error
	Close() error
	Chmod(os.FileMode) error
	Name() string
}

type pricingFileOps struct {
	createTemp func(string, string) (pricingTempFile, error)
	stat       func(string) (os.FileInfo, error)
	rename     func(string, string) error
	remove     func(string) error
}

func writeAtomicPricingFile(path string, body []byte) error {
	return writeAtomicPricingFileWithOps(path, body, pricingFileOps{
		createTemp: func(dir, pattern string) (pricingTempFile, error) { return os.CreateTemp(dir, pattern) },
		stat:       os.Stat, rename: os.Rename, remove: os.Remove,
	})
}

// Each file is replaced atomically. This is deliberately not a two-file transaction:
// a restart always derives the authoritative hash from the actual JSON body.
func writeAtomicPricingFileWithOps(path string, body []byte, ops pricingFileOps) error {
	mode := os.FileMode(0644)
	if info, err := ops.stat(path); err == nil {
		mode = info.Mode().Perm()
	} else if !os.IsNotExist(err) {
		return err
	}
	f, err := ops.createTemp(filepath.Dir(path), ".pricing-*")
	if err != nil {
		return err
	}
	closed := false
	defer func() {
		if !closed {
			_ = f.Close()
		}
		_ = ops.remove(f.Name())
	}()
	if err := f.Chmod(mode); err != nil {
		return err
	}
	n, err := f.Write(body)
	if err != nil {
		return err
	}
	if n != len(body) {
		return io.ErrShortWrite
	}
	if err := f.Sync(); err != nil {
		return err
	}
	err = f.Close()
	closed = true
	if err != nil {
		return err
	}
	return ops.rename(f.Name(), path)
}

// Caller holds updateMu. Retain the pending body until both replacements finish.
func (s *PricingService) retryPricingPersistenceLocked() error {
	if s.pendingPricingBody == nil {
		return nil
	}
	body := s.pendingPricingBody
	if err := writeAtomicPricingFile(s.getPricingFilePath(), body); err != nil {
		return err
	}
	hash := sha256.Sum256(body)
	if err := writeAtomicPricingFile(s.getHashFilePath(), []byte(hex.EncodeToString(hash[:])+"\n")); err != nil {
		return err
	}
	s.pendingPricingBody = nil
	return nil
}
