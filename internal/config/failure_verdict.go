package config

import "fmt"

// FailureVerdictPolicy controls the bot's standing rejection after operational failure.
type FailureVerdictPolicy string

const (
	// DismissLatestFailureVerdict removes only the latest own requested-changes decision.
	DismissLatestFailureVerdict FailureVerdictPolicy = "dismiss_latest_block"
	// PreserveFailureVerdict retains review decisions after an operational failure.
	PreserveFailureVerdict FailureVerdictPolicy = "preserve"
)

func loadFailureVerdictPolicy(lookup LookupEnv) (FailureVerdictPolicy, error) {
	raw, found := lookup("SERVICE_FAILURE_VERDICT_POLICY")
	if !found {
		return DismissLatestFailureVerdict, nil
	}
	policy := FailureVerdictPolicy(raw)
	switch policy {
	case DismissLatestFailureVerdict, PreserveFailureVerdict:
		return policy, nil
	default:
		return "", fmt.Errorf("SERVICE_FAILURE_VERDICT_POLICY must be %q or %q", DismissLatestFailureVerdict, PreserveFailureVerdict)
	}
}
