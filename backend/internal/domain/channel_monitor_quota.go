package domain

import "time"

type MonitorQuotaTier struct {
	Window      string  `json:"window"`
	Label       string  `json:"label,omitempty"`
	UsedPercent float64 `json:"used_percent"`
	Used        float64 `json:"used,omitempty"`
	Limit       float64 `json:"limit,omitempty"`
	ResetAt     string  `json:"reset_at,omitempty"`
}

type MonitorBalance struct {
	Currency string  `json:"currency"`
	Balance  float64 `json:"balance"`
}

type MonitorQuotaSnapshot struct {
	Source            string             `json:"source"`
	Success           bool               `json:"success"`
	Tiers             []MonitorQuotaTier `json:"tiers,omitempty"`
	Balance           *float64           `json:"balance,omitempty"`
	Balances          []MonitorBalance   `json:"balances,omitempty"`
	Currency          string             `json:"currency,omitempty"`
	BalanceLow        bool               `json:"balance_low,omitempty"`
	PlanLevel         string             `json:"plan_level,omitempty"`
	CredentialInvalid bool               `json:"credential_invalid,omitempty"`
	Error             string             `json:"error,omitempty"`
	FetchedAt         time.Time          `json:"fetched_at"`
}
