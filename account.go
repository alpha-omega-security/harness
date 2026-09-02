package harness

import (
	"strings"
	"time"
)

const (
	accountPhraseRateLimit       = "rate limit"
	accountCodeRateLimit         = "rate_limit"
	accountPhraseTooManyRequests = "too many requests"
)

// AccountError reports a provider-level account problem for which immediately
// retrying the command is unlikely to help.
type AccountError struct {
	Detail  string
	ResetAt *time.Time
}

func (e *AccountError) Error() string {
	if e.Detail == "" {
		return "model API account unavailable"
	}
	return "model API account unavailable: " + e.Detail
}

// transientLimitPhrases identify limits that may recover after a reset time.
var transientLimitPhrases = []string{
	"usage limit",
	"session limit",
	"plan limit",
	accountPhraseRateLimit,
	accountCodeRateLimit,
	accountPhraseTooManyRequests,
	"quota exceeded",
	"credit balance",
	"429",
}

// accessRevokedPhrases identify account problems that need operator action,
// not an automatic retry after a reset time.
var accessRevokedPhrases = []string{
	"disabled claude subscription access",
	"use an anthropic api key instead",
	"ask your admin to enable access",
	"access has been revoked",
}

// matchAccountPhrase returns the trimmed input when any phrase matches. Callers
// should invoke it only after a backend exits non-zero; otherwise ordinary
// model output containing a phrase such as "rate limit" could be misclassified.
func matchAccountPhrase(s string, lists ...[]string) string {
	text := strings.TrimSpace(s)
	if text == "" {
		return ""
	}
	lower := strings.ToLower(text)
	for _, list := range lists {
		for _, phrase := range list {
			if strings.Contains(lower, phrase) {
				return text
			}
		}
	}
	return ""
}

// claudeAccountErrorText recognises Claude usage limits and revoked access.
func claudeAccountErrorText(s string) string {
	return matchAccountPhrase(s, transientLimitPhrases, accessRevokedPhrases)
}

// accountErrorAccessRevoked detects permanent account failures that must never
// drive an automatic resume, even if the same message mentions a limit.
func accountErrorAccessRevoked(s string) bool {
	lower := strings.ToLower(s)
	for _, phrase := range accessRevokedPhrases {
		if strings.Contains(lower, phrase) {
			return true
		}
	}
	return false
}

// AccountErrorResumable reports whether an account error describes a transient
// limit. Revoked access wins when both permanent and transient phrases appear.
func AccountErrorResumable(s string) bool {
	if accountErrorAccessRevoked(s) {
		return false
	}
	lower := strings.ToLower(s)
	for _, phrase := range transientLimitPhrases {
		if strings.Contains(lower, phrase) {
			return true
		}
	}
	return false
}

// PreferAccountErrorText keeps the first account error unless a later message
// is non-resumable while the earlier one is a transient limit. Keying on
// AccountErrorResumable rather than the shared revoked-phrase list means a
// backend-local permanent phrase such as "invalid_api_key" still displaces an
// earlier "rate limit", so a permanent failure is never scheduled for retry.
func PreferAccountErrorText(current, candidate string) string {
	switch {
	case candidate == "":
		return current
	case current == "":
		return candidate
	case !AccountErrorResumable(candidate) && AccountErrorResumable(current):
		return candidate
	default:
		return current
	}
}

// PreferRateLimitReset returns the rejected rate limit with the later reset so
// a retry is not scheduled while another reported window still blocks use.
func PreferRateLimitReset(current, candidate *RateLimitInfo) *RateLimitInfo {
	if candidate == nil || !candidate.Rejected() || candidate.ResetTime() == nil {
		return current
	}
	if current == nil {
		return candidate
	}
	currentReset := current.ResetTime()
	candidateReset := candidate.ResetTime()
	if currentReset == nil || candidateReset.After(*currentReset) {
		return candidate
	}
	return current
}

// ResumableReset returns a rejected limit's reset time only when the associated
// account error is transient. Revoked access always requires manual action.
func ResumableReset(errText string, limit *RateLimitInfo) *time.Time {
	if !AccountErrorResumable(errText) || limit == nil || !limit.Rejected() {
		return nil
	}
	return limit.ResetTime()
}
