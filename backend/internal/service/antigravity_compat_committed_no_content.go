package service

import "errors"

var errAntigravityCommittedNoContent = errors.New("antigravity committed stream ended without content")

func IsAntigravityCommittedNoContent(err error) bool {
	return errors.Is(err, errAntigravityCommittedNoContent)
}
