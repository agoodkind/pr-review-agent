package quota

import (
	"errors"
	"time"
)

// ReportPolicy bounds delivery separately from the model request that produced usage.
type ReportPolicy struct {
	Attempts       int
	AttemptTimeout time.Duration
	Timeout        time.Duration
}

// DefaultReportPolicy applies when callers omit delivery settings.
func DefaultReportPolicy() ReportPolicy {
	return ReportPolicy{Attempts: 3, AttemptTimeout: 5 * time.Second, Timeout: 20 * time.Second}
}

// Validate rejects policies that cannot perform one complete delivery attempt.
func (policy ReportPolicy) Validate() error {
	if policy.Attempts <= 0 {
		return errors.New("report attempts must be positive")
	}
	if policy.AttemptTimeout <= 0 {
		return errors.New("report attempt_timeout must be positive")
	}
	if policy.Timeout < policy.AttemptTimeout {
		return errors.New("report timeout must be at least attempt_timeout")
	}
	return nil
}
