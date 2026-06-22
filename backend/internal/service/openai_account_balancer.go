package service

import (
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
