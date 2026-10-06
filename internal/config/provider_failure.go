package config

import (
	"fmt"
	"time"
)

// ProviderFailurePolicy controls fallback after response validation as well as transport errors.
type ProviderFailurePolicy string

const (
	// ProviderAnyFailure includes responses that fail decoding or validation.
	ProviderAnyFailure ProviderFailurePolicy = "any_failure"
	// ProviderRecoverableFailure preserves quota, throttle, and availability fallback.
	ProviderRecoverableFailure ProviderFailurePolicy = "recoverable"
)

// ResolveProviderFailurePolicy preserves recoverable behavior for callers without an explicit policy.
func ResolveProviderFailurePolicy(policy ProviderFailurePolicy) ProviderFailurePolicy {
	if policy == "" {
		return ProviderRecoverableFailure
	}
	return policy
}

// LoadProviderFailurePolicy rejects unknown policy values before provider requests.
func LoadProviderFailurePolicy(lookup LookupEnv) (ProviderFailurePolicy, error) {
	value, _ := lookup("PROVIDER_FAILURE_POLICY")
	policy := ResolveProviderFailurePolicy(ProviderFailurePolicy(value))
	switch policy {
	case ProviderAnyFailure, ProviderRecoverableFailure:
		return policy, nil
	default:
		return "", fmt.Errorf("PROVIDER_FAILURE_POLICY must be any_failure or recoverable")
	}
}

func providerRequestTimeout(definition providerDefinition) (time.Duration, error) {
	if definition.RequestTimeout == "" {
		return 0, nil
	}
	timeout, err := time.ParseDuration(definition.RequestTimeout)
	if err != nil || timeout <= 0 {
		return 0, fmt.Errorf("provider %q request_timeout must be a positive duration", definition.ID)
	}
	return timeout, nil
}
