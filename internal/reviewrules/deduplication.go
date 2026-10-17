package reviewrules

// Deduplication selects whether findings identify a defect or a source location.
type Deduplication string

const (
	// DeduplicateDefect suppresses repeated reports of the same defect.
	DeduplicateDefect Deduplication = "defect"
	// DeduplicateLocation suppresses repeated reports at the same source range.
	DeduplicateLocation Deduplication = "location"
)

// FindingDeduplication applies the configured rule policy before publication.
func (catalog Catalog) FindingDeduplication(id ID) (Deduplication, error) {
	entry, err := catalog.findRule(id)
	if err != nil {
		return "", err
	}
	if entry.Deduplication == "" {
		return DeduplicateDefect, nil
	}
	return entry.Deduplication, nil
}
