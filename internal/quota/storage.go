package quota

import "context"

type storageRequest struct {
	Action string `json:"action"`
}

// StorageStats reports database allocation and retained usage rows without SDK contents.
type StorageStats struct {
	DatabaseBytes     int64          `json:"database_bytes"`
	Events            int64          `json:"events"`
	Details           int64          `json:"details"`
	SDKRecords        int64          `json:"sdk_records"`
	TotalsBuckets     int64          `json:"totals_buckets"`
	TotalsGroups      int64          `json:"totals_groups"`
	SDKPayloadBytes   int64          `json:"sdk_payload_bytes"`
	OldestAdmissionMS *int64         `json:"oldest_admission_ms"`
	NewestAdmissionMS *int64         `json:"newest_admission_ms"`
	Retention         RetentionStats `json:"retention"`
}

// RetentionStats includes the protected quota horizon and irreversible history boundary.
type RetentionStats struct {
	Enabled              bool   `json:"enabled"`
	ConfiguredMaxAgeMS   int64  `json:"configured_max_age_ms"`
	EffectiveMaxAgeMS    int64  `json:"effective_max_age_ms"`
	CleanupIntervalMS    int64  `json:"cleanup_interval_ms"`
	BatchSize            int64  `json:"batch_size"`
	CutoffMS             int64  `json:"cutoff_ms"`
	NextMaintenanceMS    *int64 `json:"next_maintenance_ms"`
	SDKMigrationComplete bool   `json:"sdk_migration_complete"`
}

// Storage reads the existing authenticated provider-budget endpoint.
func (client *Client) Storage(ctx context.Context) (StorageStats, error) {
	var result StorageStats
	err := post(ctx, client, storageRequest{Action: "storage_stats"}, &result)
	return result, err
}
