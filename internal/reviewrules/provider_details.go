package reviewrules

import "fmt"

// ProviderDetailColumn selects one operator diagnostic column.
type ProviderDetailColumn string

const (
	// ProviderIdentityColumn displays the configured provider identifier.
	ProviderIdentityColumn ProviderDetailColumn = "provider"
	// ProviderModelsColumn distinguishes requested models from reported models.
	ProviderModelsColumn ProviderDetailColumn = "models"
	// ProviderSettingsColumn displays API, configured reasoning, and output cap.
	ProviderSettingsColumn ProviderDetailColumn = "settings"
	// ProviderRequestsColumn distinguishes attempts, API calls, and completions.
	ProviderRequestsColumn ProviderDetailColumn = "requests"
	// ProviderTokensColumn displays provider-reported token usage.
	ProviderTokensColumn ProviderDetailColumn = "tokens"
	// ProviderCostColumn displays the estimate and its priced request count.
	ProviderCostColumn ProviderDetailColumn = "cost"
	// ProviderQuotaColumn displays the app quota before the last request.
	ProviderQuotaColumn ProviderDetailColumn = "quota"
	// ProviderResultColumn displays typed outcomes and fallback counts.
	ProviderResultColumn ProviderDetailColumn = "result"
)

func validateProviderColumns(columns []ProviderDetailColumn) error {
	if len(columns) == 0 {
		return fmt.Errorf("provider_details requires at least one diagnostic column")
	}
	seen := make(map[ProviderDetailColumn]struct{}, len(columns))
	for _, column := range columns {
		switch column {
		case ProviderIdentityColumn, ProviderModelsColumn, ProviderSettingsColumn, ProviderRequestsColumn,
			ProviderTokensColumn, ProviderCostColumn, ProviderQuotaColumn, ProviderResultColumn:
		default:
			return fmt.Errorf("unknown provider_details column %q", column)
		}
		if _, duplicate := seen[column]; duplicate {
			return fmt.Errorf("duplicate provider_details column %q", column)
		}
		seen[column] = struct{}{}
	}
	return nil
}
