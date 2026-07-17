package service

import "time"

type AccountGroup struct {
	AccountID                         int64
	GroupID                           int64
	Priority                          int
	UpstreamBillingGuardMaxMultiplier *float64
	CreatedAt                         time.Time

	Account *Account
	Group   *Group
}
