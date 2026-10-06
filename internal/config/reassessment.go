package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Reassessment permits indefinite retries when both attempt and age limits are zero.
type Reassessment struct {
	ReadBudget           int    `json:"read_budget"`
	ReconcileInterval    string `json:"reconcile_interval"`
	PageSize             int    `json:"page_size"`
	BatchSize            int    `json:"batch_size"`
	StateConfirmations   int    `json:"state_confirmations"`
	ConfirmationInterval string `json:"confirmation_interval"`
	Enabled              bool   `json:"enabled"`
	QueueURL             string `json:"queue_url"`
	InitialDelay         string `json:"initial_delay"`
	MaximumDelay         string `json:"maximum_delay"`
	MaximumAttempts      int    `json:"maximum_attempts"`
	TTL                  string `json:"ttl"`
	TerminalRetention    string `json:"terminal_retention"`
	RetryDeclined        bool   `json:"retry_declined"`
}

func loadReassessment(lookup LookupEnv) (Reassessment, error) {
	var settings Reassessment
	raw, found := lookup("REASSESSMENT")
	if !found {
		return settings, nil
	}
	if err := json.Unmarshal([]byte(raw), &settings); err != nil {
		return settings, errors.New("REASSESSMENT must be an object")
	}
	if !settings.Enabled {
		return settings, nil
	}
	if _, err := parseRequiredHTTPSURL("REASSESSMENT.queue_url", settings.QueueURL); err != nil {
		return settings, err
	}
	initial, err := time.ParseDuration(settings.InitialDelay)
	if err != nil || initial <= 0 {
		return settings, errors.New("REASSESSMENT.initial_delay must be positive")
	}
	maximum, err := time.ParseDuration(settings.MaximumDelay)
	if err != nil || maximum < initial {
		return settings, errors.New("REASSESSMENT.maximum_delay must be at least initial_delay")
	}
	for name, value := range map[string]string{"ttl": settings.TTL, "terminal_retention": settings.TerminalRetention} {
		duration, parseErr := time.ParseDuration(value)
		if parseErr != nil || duration < 0 {
			return settings, fmt.Errorf("REASSESSMENT.%s must be a nonnegative duration", name)
		}
	}
	if settings.MaximumAttempts < 0 {
		return settings, errors.New("REASSESSMENT.maximum_attempts must be nonnegative")
	}
	for name, value := range map[string]string{"reconcile_interval": settings.ReconcileInterval, "confirmation_interval": settings.ConfirmationInterval} {
		duration, parseErr := time.ParseDuration(value)
		if parseErr != nil || duration <= 0 {
			return settings, fmt.Errorf("REASSESSMENT.%s must be positive", name)
		}
	}
	if settings.PageSize < 1 || settings.PageSize > 100 || settings.BatchSize < 1 || settings.StateConfirmations < 1 || settings.ReadBudget < 1 {
		return settings, errors.New("REASSESSMENT requires page_size from 1 through 100 and positive batch_size and state_confirmations")
	}
	return settings, nil
}
