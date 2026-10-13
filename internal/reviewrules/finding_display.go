package reviewrules

import "fmt"

// FindingDisplay controls report presentation without changing review decisions.
type FindingDisplay string

const (
	// FindingsExpanded includes metadata findings in the visible report body.
	FindingsExpanded FindingDisplay = "expanded"
	// FindingsCollapsed includes metadata findings inside a details disclosure.
	FindingsCollapsed FindingDisplay = "collapsed"
	// FindingsHidden omits metadata finding text from the report body.
	FindingsHidden FindingDisplay = "hidden"
)

func validateFindingDisplay(display FindingDisplay) error {
	switch display {
	case FindingsExpanded, FindingsCollapsed, FindingsHidden:
		return nil
	case "":
		return fmt.Errorf("prompt configuration requires metadata_findings_display; set expanded, collapsed, or hidden")
	default:
		return fmt.Errorf("metadata_findings_display %q is invalid; set expanded, collapsed, or hidden", display)
	}
}

// MetadataFindingsDisplay supplies the loaded presentation mode for metadata findings.
func (policy Policy) MetadataFindingsDisplay() FindingDisplay {
	return policy.metadataFindingsDisplay
}
