package service

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

const billingNotifyWorkers = 4
const billingNotifyQueueSize = 128

type billingNotifyJob struct {
	user                    *User // Newly constructed notification-only value; no request/User aliases.
	oldBalance, balanceCost float64
	accountID               int64
	accountName, platform   string
	quotaDims               []quotaDim
	quotaCost               float64
	refreshQuota            bool
}
type billingNotifyWorker struct {
	mu      sync.RWMutex
	closed  bool
	jobs    chan billingNotifyJob
	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	dropped atomic.Uint64
}

func newBillingNotifyWorker(s *BalanceNotifyService) *billingNotifyWorker {
	ctx, cancel := context.WithCancel(context.Background())
	w := &billingNotifyWorker{jobs: make(chan billingNotifyJob, billingNotifyQueueSize), ctx: ctx, cancel: cancel}
	for range billingNotifyWorkers {
		w.wg.Go(func() {
			for {
				select {
				case <-ctx.Done():
					return
				case job := <-w.jobs:
					func() {
						defer func() {
							if r := recover(); r != nil {
								slog.Error("billing notification task panic")
							}
						}()
						task, cancel := context.WithTimeout(ctx, 30*time.Second)
						defer cancel()
						if task.Err() != nil {
							w.dropped.Add(1)
							return
						}
						s.runBillingNotification(task, job)
					}()
				}
			}
		})
	}
	return w
}
func (w *billingNotifyWorker) submit(job billingNotifyJob) bool {
	w.mu.RLock()
	defer w.mu.RUnlock()
	if w.closed {
		w.dropped.Add(1)
		return false
	}
	select {
	case w.jobs <- job:
		return true
	default:
		w.dropped.Add(1)
		return false
	}
}
func (s *BalanceNotifyService) Stop() {
	if s == nil || s.worker == nil {
		return
	}
	w := s.worker
	w.mu.Lock()
	w.closed = true
	w.cancel()
	w.mu.Unlock()
	w.wg.Wait()
	for {
		select {
		case <-w.jobs:
			w.dropped.Add(1)
		default:
			return
		}
	}
}
func (s *BalanceNotifyService) NotificationDropped() uint64 {
	if s == nil || s.worker == nil {
		return 0
	}
	return s.worker.dropped.Load()
}

func (s *BalanceNotifyService) enqueueBillingNotification(p *postUsageBillingParams, result *UsageBillingApplyResult) {
	if s == nil || s.worker == nil || p == nil || p.Cost == nil {
		return
	}
	job := billingNotifyJob{}
	if !p.IsSubscriptionBill && p.Cost.ActualCost > 0 && p.User != nil && p.User.BalanceNotifyEnabled {
		u := p.User
		job.user = &User{ID: u.ID, Email: u.Email, Username: u.Username, BalanceNotifyEnabled: true,
			BalanceNotifyThresholdType: u.BalanceNotifyThresholdType, BalanceNotifyThreshold: cloneUsageValue(u.BalanceNotifyThreshold),
			BalanceNotifyExtraEmails: append([]NotifyEmailEntry(nil), u.BalanceNotifyExtraEmails...), TotalRecharged: u.TotalRecharged}
		job.oldBalance, job.balanceCost = resolveOldBalance(p, result), p.Cost.ActualCost
	}
	if p.Cost.TotalCost > 0 && p.Account != nil && p.Account.SupportsAccountQuota() {
		a := p.Account
		job.accountID, job.accountName, job.platform = a.ID, a.Name, a.Platform
		job.quotaCost = p.Cost.TotalCost * p.AccountRateMultiplier
		if result != nil && result.QuotaState != nil {
			job.quotaDims = buildQuotaDimsFromState(a, result.QuotaState)
		} else {
			job.quotaDims = buildQuotaDims(a)
			job.refreshQuota = true
		}
	}
	if job.user != nil || len(job.quotaDims) > 0 {
		s.worker.submit(job)
	}
}
func (s *BalanceNotifyService) runBillingNotification(ctx context.Context, job billingNotifyJob) {
	if s.emailService == nil || s.settingRepo == nil {
		return
	}
	// One task-local batch; a later settings save is observed by the next task.
	keys := []string{SettingKeyBalanceLowNotifyEnabled, SettingKeyBalanceLowNotifyThreshold, SettingKeyBalanceLowNotifyRechargeURL, SettingKeyAccountQuotaNotifyEnabled, SettingKeyAccountQuotaNotifyEmails, SettingKeySiteName}
	settings, err := s.settingRepo.GetMultiple(ctx, keys)
	if err != nil || ctx.Err() != nil {
		return
	}
	ctx = context.WithValue(ctx, balanceNotifySettingsKey{}, settings)
	if job.user != nil {
		s.CheckBalanceAfterDeduction(ctx, job.user, job.oldBalance, job.balanceCost)
	}
	if job.quotaCost > 0 && settings[SettingKeyAccountQuotaNotifyEnabled] == "true" {
		recipients := filterVerifiedEmails(ParseNotifyEmails(settings[SettingKeyAccountQuotaNotifyEmails]))
		if len(recipients) > 0 {
			a := &Account{ID: job.accountID, Name: job.accountName, Platform: job.platform}
			if job.refreshQuota && s.accountRepo != nil {
				if fresh, err := s.accountRepo.GetByID(ctx, job.accountID); err == nil && fresh != nil {
					a = fresh
					job.quotaDims = buildQuotaDims(fresh)
				}
			}
			s.checkQuotaDimCrossings(a, job.quotaDims, job.quotaCost, recipients, s.getSiteName(ctx), ctx)
		}
	}
}

type balanceNotifySettingsKey struct{}

func notificationTaskContext(contexts []context.Context) context.Context {
	if len(contexts) > 0 && contexts[0] != nil {
		return contexts[0]
	}
	return context.Background()
}

func (s *BalanceNotifyService) notificationSettings(ctx context.Context, keys []string) (map[string]string, error) {
	if settings, ok := ctx.Value(balanceNotifySettingsKey{}).(map[string]string); ok {
		return settings, nil
	}
	return s.settingRepo.GetMultiple(ctx, keys)
}
func (s *BalanceNotifyService) notificationSetting(ctx context.Context, key string) (string, error) {
	if settings, ok := ctx.Value(balanceNotifySettingsKey{}).(map[string]string); ok {
		return settings[key], nil
	}
	return s.settingRepo.GetValue(ctx, key)
}
