package config

import (
	"errors"
	"time"
)

// DefaultProviderUsageQueryTimeout bounds one display query when configuration omits it.
const DefaultProviderUsageQueryTimeout = 15 * time.Second

// UsageQueryTimeout returns the display query timeout independently of quota admission.
func (cfg Config) UsageQueryTimeout() time.Duration {
	if cfg.ProviderUsageQueryTimeout == 0 {
		return DefaultProviderUsageQueryTimeout
	}
	return cfg.ProviderUsageQueryTimeout
}

func loadUsageQueryTimeout(lookup LookupEnv) (time.Duration, error) {
	raw, configured := lookup("PROVIDER_USAGE_QUERY_TIMEOUT")
	if !configured {
		return DefaultProviderUsageQueryTimeout, nil
	}
	timeout, err := time.ParseDuration(raw)
	if err != nil || timeout <= 0 {
		return 0, errors.New("PROVIDER_USAGE_QUERY_TIMEOUT must be a positive duration")
	}
	return timeout, nil
}
