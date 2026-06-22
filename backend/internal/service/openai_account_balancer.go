package service

import (
	"sort"
	"sync/atomic"
	"time"
)

var openAIAccountBalanceSeedCounter atomic.Uint64

func shuffleOpenAIAccountLoadTies(accounts []accountWithLoad) {
	shuffleOpenAIAccountLoadTiesWithReset(accounts, false)
}

func shuffleOpenAIAccountLoadTiesWithReset(accounts []accountWithLoad, includeReset bool) {
	if len(accounts) <= 1 {
		return
	}
	rng := newOpenAISelectionRNG(nextOpenAIAccountBalanceSeed())
	for i := 0; i < len(accounts); {
		j := i + 1
		for j < len(accounts) && sameOpenAIAccountLoadTie(accounts[i], accounts[j], includeReset) {
			j++
		}
		shuffleOpenAIAccountLoadRange(accounts[i:j], &rng)
		i = j
	}
}

func sameOpenAIAccountLoadTie(a, b accountWithLoad, includeReset bool) bool {
	if a.account == nil || b.account == nil || a.loadInfo == nil || b.loadInfo == nil {
		return false
	}
	if includeReset && !sameOpenAIAccountResetTie(a.account, b.account) {
		return false
	}
	return a.account.Priority == b.account.Priority &&
		a.account.IsPoolMode() == b.account.IsPoolMode() &&
		a.loadInfo.LoadRate == b.loadInfo.LoadRate &&
		a.loadInfo.WaitingCount == b.loadInfo.WaitingCount &&
		sameOpenAIAccountLastUsedTie(a.account, b.account)
}

func shuffleOpenAIStrictPriorityTies(accounts []openAIAccountCandidateScore) {
	shuffleOpenAIStrictPriorityTiesWithReset(accounts, false)
}

func shuffleOpenAIStrictPriorityTiesWithReset(accounts []openAIAccountCandidateScore, includeReset bool) {
	if len(accounts) <= 1 {
		return
	}
	rng := newOpenAISelectionRNG(nextOpenAIAccountBalanceSeed())
	for i := 0; i < len(accounts); {
		j := i + 1
		for j < len(accounts) && sameOpenAIStrictPriorityTie(accounts[i], accounts[j], includeReset) {
			j++
		}
		shuffleOpenAIStrictPriorityRange(accounts[i:j], &rng)
		i = j
	}
}

func sameOpenAIStrictPriorityTie(a, b openAIAccountCandidateScore, includeReset bool) bool {
	if a.account == nil || b.account == nil || a.loadInfo == nil || b.loadInfo == nil {
		return false
	}
	if includeReset && !sameOpenAIAccountResetTie(a.account, b.account) {
		return false
	}
	return a.account.Priority == b.account.Priority &&
		a.account.IsPoolMode() == b.account.IsPoolMode() &&
		a.loadInfo.LoadRate == b.loadInfo.LoadRate &&
		a.loadInfo.WaitingCount == b.loadInfo.WaitingCount &&
		a.errorRate == b.errorRate &&
		a.hasTTFT == b.hasTTFT &&
		(!a.hasTTFT || a.ttft == b.ttft) &&
		sameOpenAIAccountLastUsedTie(a.account, b.account)
}

func sameOpenAIAccountResetTie(a, b *Account) bool {
	if a == nil || b == nil {
		return false
	}
	switch {
	case a.SessionWindowEnd == nil && b.SessionWindowEnd == nil:
		return true
	case a.SessionWindowEnd == nil || b.SessionWindowEnd == nil:
		return false
	default:
		return a.SessionWindowEnd.Equal(*b.SessionWindowEnd)
	}
}

func sameOpenAIAccountLastUsedTie(a, b *Account) bool {
	if a == nil || b == nil {
		return false
	}
	switch {
	case a.LastUsedAt == nil && b.LastUsedAt == nil:
		return true
	case a.LastUsedAt == nil || b.LastUsedAt == nil:
		return false
	default:
		return a.LastUsedAt.Equal(*b.LastUsedAt)
	}
}

func nonPoolAccountBeforePool(a, b *Account) (bool, bool) {
	if a == nil || b == nil {
		return false, false
	}
	aPool := a.IsPoolMode()
	bPool := b.IsPoolMode()
	if aPool == bPool {
		return false, false
	}
	return !aPool, true
}

func sortAccountsByPriorityPoolAndLastUsed(accounts []*Account, preferOAuth bool) {
	sort.SliceStable(accounts, func(i, j int) bool {
		a, b := accounts[i], accounts[j]
		if a.Priority != b.Priority {
			return a.Priority < b.Priority
		}
		if less, ok := nonPoolAccountBeforePool(a, b); ok {
			return less
		}
		switch {
		case a.LastUsedAt == nil && b.LastUsedAt != nil:
			return true
		case a.LastUsedAt != nil && b.LastUsedAt == nil:
			return false
		case a.LastUsedAt == nil && b.LastUsedAt == nil:
			if preferOAuth && a.Type != b.Type {
				return a.Type == AccountTypeOAuth
			}
			return false
		default:
			return a.LastUsedAt.Before(*b.LastUsedAt)
		}
	})
	shuffleAccountsWithinPriorityPoolAndLastUsed(accounts, preferOAuth)
}

func shuffleAccountsWithinPriorityPoolAndLastUsed(accounts []*Account, preferOAuth bool) {
	if len(accounts) <= 1 {
		return
	}
	rng := newOpenAISelectionRNG(nextOpenAIAccountBalanceSeed())
	for i := 0; i < len(accounts); {
		j := i + 1
		for j < len(accounts) && sameAccountPriorityPoolLastUsedTie(accounts[i], accounts[j]) {
			j++
		}
		if j-i > 1 {
			if preferOAuth {
				oauth := make([]*Account, 0, j-i)
				others := make([]*Account, 0, j-i)
				for _, acc := range accounts[i:j] {
					if acc.Type == AccountTypeOAuth {
						oauth = append(oauth, acc)
					} else {
						others = append(others, acc)
					}
				}
				shuffleAccountPointers(oauth, &rng)
				shuffleAccountPointers(others, &rng)
				copy(accounts[i:], oauth)
				copy(accounts[i+len(oauth):], others)
			} else {
				shuffleAccountPointers(accounts[i:j], &rng)
			}
		}
		i = j
	}
}

func sameAccountPriorityPoolLastUsedTie(a, b *Account) bool {
	if a == nil || b == nil {
		return false
	}
	return a.Priority == b.Priority &&
		a.IsPoolMode() == b.IsPoolMode() &&
		sameOpenAIAccountLastUsedTie(a, b)
}

func shuffleAccountPointers(items []*Account, rng *openAISelectionRNG) {
	if len(items) <= 1 || rng == nil {
		return
	}
	for i := len(items) - 1; i > 0; i-- {
		j := int(rng.nextUint64() % uint64(i+1))
		items[i], items[j] = items[j], items[i]
	}
}

func shuffleOpenAIAccountLoadRange(items []accountWithLoad, rng *openAISelectionRNG) {
	if len(items) <= 1 || rng == nil {
		return
	}
	for i := len(items) - 1; i > 0; i-- {
		j := int(rng.nextUint64() % uint64(i+1))
		items[i], items[j] = items[j], items[i]
	}
}

func shuffleOpenAIStrictPriorityRange(items []openAIAccountCandidateScore, rng *openAISelectionRNG) {
	if len(items) <= 1 || rng == nil {
		return
	}
	for i := len(items) - 1; i > 0; i-- {
		j := int(rng.nextUint64() % uint64(i+1))
		items[i], items[j] = items[j], items[i]
	}
}

func nextOpenAIAccountBalanceSeed() uint64 {
	seed := uint64(time.Now().UnixNano())
	seq := openAIAccountBalanceSeedCounter.Add(1)
	return seed ^ (seq * 0x9e3779b97f4a7c15)
}
