package main

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
)

// proxyPool names the proxy list an exit came from.
type proxyPool int

const (
	poolPrimary proxyPool = iota
	poolFallback
)

func (pool proxyPool) String() string {
	if pool == poolFallback {
		return "fallback"
	}

	return "primary"
}

// poolHeader names the proxy list whose exit produced the final response.
const poolHeader = "X-Fpm-Pool"

// blockBodyScanLimit bounds how much of a body is matched against the block pattern: block pages put
// their marker in the head, and real pages can be megabytes.
const blockBodyScanLimit = 128 << 10

// exitPools holds the ready exits of each proxy list.
type exitPools struct {
	primary  []proxyExit
	fallback []proxyExit
}

func (pools exitPools) of(pool proxyPool) []proxyExit {
	if pool == poolFallback {
		return pools.fallback
	}

	return pools.primary
}

// poolUpdate replaces the ready exits of one pool.
type poolUpdate struct {
	pool  proxyPool
	exits []proxyExit
}

// fallbackPolicy moves a request from the primary proxy list to the fallback list after its primary attempts
// were blocked by anti-bot measures (captcha, interstitial, 403/429).
type fallbackPolicy struct {
	// afterBlocks is how many blocked primary attempts move a request to the fallback pool.
	afterBlocks int
	// attempts is how many blocked fallback attempts a request may have before its block is final.
	attempts int
	// blockCodes are target status codes that count as blocked.
	blockCodes []int
	// blockBody matches block pages served with any status (e.g. a 200 "Pardon Our Interruption").
	blockBody *regexp.Regexp
}

// newFallbackPolicy validates the FALLBACK_* and BLOCK_* settings.
// Edge cases: afterBlocks above retries+1 could never be reached, since blocked primary attempts use the
// RETRIES budget; an empty pattern disables body matching.
func newFallbackPolicy(afterBlocks int, attempts int, blockCodes []int, blockBody string, retries int) (*fallbackPolicy, error) {
	if afterBlocks < 1 || afterBlocks > retries+1 {
		return nil, fmt.Errorf("FALLBACK_AFTER_BLOCKS must be between 1 and RETRIES+1 (%d), got %d", retries+1, afterBlocks)
	}

	if attempts < 1 {
		return nil, fmt.Errorf("FALLBACK_ATTEMPTS must be positive, got %d", attempts)
	}

	if len(blockCodes) == 0 && blockBody == "" {
		return nil, errors.New("BLOCK_STATUS_CODES and BLOCK_BODY_PATTERN are both empty: nothing would count as blocked")
	}

	for _, code := range blockCodes {
		if code < 100 || code > 599 {
			return nil, fmt.Errorf("BLOCK_STATUS_CODES: %d is not an HTTP status code", code)
		}
	}

	policy := &fallbackPolicy{afterBlocks: afterBlocks, attempts: attempts, blockCodes: slices.Clone(blockCodes)}

	if blockBody != "" {
		pattern, err := regexp.Compile(blockBody)
		if err != nil {
			return nil, fmt.Errorf("BLOCK_BODY_PATTERN: %w", err)
		}

		policy.blockBody = pattern
	}

	return policy, nil
}

// isBlocked reports whether the target answered response to turn the client away.
// Edge cases: only target answers count; proxy failures, timeouts and cancellations are not blocks.
func (policy *fallbackPolicy) isBlocked(response *Response) bool {
	if response == nil || response.Status != ResponseStatusOk {
		return false
	}

	if slices.Contains(policy.blockCodes, response.Code) {
		return true
	}

	if policy.blockBody == nil {
		return false
	}

	return policy.blockBody.Match(response.Body[:min(len(response.Body), blockBodyScanLimit)])
}

// decide adjusts decision, made by classifyAttempt for final, for a blocked response, updating the request's
// block counters and pool. The caller holds request.Lock.
// A blocked primary attempt is retried (using RETRIES) until afterBlocks of them moved the request to the
// fallback pool; there, blocked attempts are re-queued without using RETRIES until attempts ran out.
// Edge cases: a nil policy or an unblocked response keeps decision; without ready fallback exits the
// request stays on the primary pool.
func (policy *fallbackPolicy) decide(request *ActiveRequest, final *Response, decision attemptDecision, fallbackReady bool) attemptDecision {
	if policy == nil || !policy.isBlocked(final) {
		return decision
	}

	if request.Pool == poolFallback {
		request.FallbackBlocks++
		if request.FallbackBlocks < policy.attempts {
			return decisionFallback
		}

		return decisionFinish
	}

	request.PrimaryBlocks++
	if request.PrimaryBlocks >= policy.afterBlocks && fallbackReady {
		request.Pool = poolFallback

		return decisionFallback
	}

	return decisionRetry
}
