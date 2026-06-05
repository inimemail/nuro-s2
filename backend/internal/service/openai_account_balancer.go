package service

import (
	"sync/atomic"
	"time"
)

var openAIAccountBalanceSeedCounter atomic.Uint64

func shuffleOpenAIAccountLoadTies(accounts []accountWithLoad) {
	if len(accounts) <= 1 {
		return
	}
	rng := newOpenAISelectionRNG(nextOpenAIAccountBalanceSeed())
	for i := 0; i < len(accounts); {
		j := i + 1
		for j < len(accounts) && sameOpenAIAccountLoadTie(accounts[i], accounts[j]) {
			j++
		}
		shuffleOpenAIAccountLoadRange(accounts[i:j], &rng)
		i = j
	}
}

func sameOpenAIAccountLoadTie(a, b accountWithLoad) bool {
	if a.account == nil || b.account == nil || a.loadInfo == nil || b.loadInfo == nil {
		return false
	}
	return a.account.Priority == b.account.Priority &&
		a.loadInfo.LoadRate == b.loadInfo.LoadRate &&
		a.loadInfo.WaitingCount == b.loadInfo.WaitingCount
}

func shuffleOpenAIStrictPriorityTies(accounts []openAIAccountCandidateScore) {
	if len(accounts) <= 1 {
		return
	}
	rng := newOpenAISelectionRNG(nextOpenAIAccountBalanceSeed())
	for i := 0; i < len(accounts); {
		j := i + 1
		for j < len(accounts) && sameOpenAIStrictPriorityTie(accounts[i], accounts[j]) {
			j++
		}
		shuffleOpenAIStrictPriorityRange(accounts[i:j], &rng)
		i = j
	}
}

func sameOpenAIStrictPriorityTie(a, b openAIAccountCandidateScore) bool {
	if a.account == nil || b.account == nil || a.loadInfo == nil || b.loadInfo == nil {
		return false
	}
	return a.account.Priority == b.account.Priority &&
		a.loadInfo.LoadRate == b.loadInfo.LoadRate &&
		a.loadInfo.WaitingCount == b.loadInfo.WaitingCount &&
		a.errorRate == b.errorRate &&
		a.hasTTFT == b.hasTTFT &&
		(!a.hasTTFT || a.ttft == b.ttft)
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
