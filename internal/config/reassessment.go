package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Reassessment permits indefinite retries when both attempt and age limits are zero.
type Reassessment struct {
	Enabled           bool   `json:"enabled"`
	QueueURL          string `json:"queue_url"`
	InitialDelay      string `json:"initial_delay"`
	MaximumDelay      string `json:"maximum_delay"`
	MaximumAttempts   int    `json:"maximum_attempts"`
	TTL               string `json:"ttl"`
	TerminalRetention string `json:"terminal_retention"`
	RetryDeclined     bool   `json:"retry_declined"`
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
	return settings, nil
}
