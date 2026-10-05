// Package reviewrules applies catalog importance before findings are filtered.
package reviewrules

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"
)

// ID remains stable when a rule's title or instructions change.
type ID string

// Importance overrides model scores before publication. Prose scores cannot be reduced.
type Importance map[ID]int

const (
	// MinimumImportance excludes zero from configured and reported scores.
	MinimumImportance = 1
	// MaximumImportance is mandatory for every verified prose violation.
	MaximumImportance = 10
)

type category string

const (
	proseCategory     category = "prose"
	technicalCategory category = "technical"
)

type rule struct {
	ID           ID       `json:"id"`
	Category     category `json:"category"`
	Title        string   `json:"title"`
	Instructions string   `json:"instructions"`
}

type catalog struct {
	Preamble string `json:"preamble"`
	Rules    []rule `json:"rules"`
}

//go:embed rules.json
var catalogJSON []byte

var rules, catalogError = loadCatalog()

func loadCatalog() (catalog, error) {
	var parsed catalog
	if err := json.Unmarshal(catalogJSON, &parsed); err != nil {
		slog.Error("decode embedded review rules", "error", err)
		return catalog{}, fmt.Errorf("decode embedded review rules: %w", err)
	}
	seen := make(map[ID]struct{}, len(parsed.Rules))
	for _, entry := range parsed.Rules {
		if entry.ID == "" || entry.Title == "" || entry.Instructions == "" {
			return catalog{}, fmt.Errorf("embedded review rule requires an ID, title, and instructions")
		}
		if entry.Category != proseCategory && entry.Category != technicalCategory {
			return catalog{}, fmt.Errorf("review rule %q has invalid category %q", entry.ID, entry.Category)
		}
		if _, duplicate := seen[entry.ID]; duplicate {
			return catalog{}, fmt.Errorf("duplicate review rule ID %q", entry.ID)
		}
		seen[entry.ID] = struct{}{}
	}
	return parsed, nil
}

// IDs preserves catalog order in a separate slice for structured-output enums.
func IDs() []ID {
	ids := make([]ID, 0, len(rules.Rules))
	for _, entry := range rules.Rules {
		ids = append(ids, entry.ID)
	}
	return ids
}

// ValidateImportance rejects unknown IDs and attempts to reduce prose importance.
func ValidateImportance(overrides Importance) error {
	if catalogError != nil {
		return catalogError
	}
	ids := make([]ID, 0, len(overrides))
	for id := range overrides {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		entry, err := findRule(id)
		if err != nil {
			return err
		}
		importance := overrides[id]
		if importance < MinimumImportance || importance > MaximumImportance {
			return fmt.Errorf("review rule %q importance must be between 1 and %d", id, MaximumImportance)
		}
		if entry.Category == proseCategory && importance != MaximumImportance {
			return fmt.Errorf("prose rule %q importance must be %d", id, MaximumImportance)
		}
	}
	return nil
}

// FindingImportance applies validated overrides and ignores reported prose severity.
func FindingImportance(id ID, reported int, overrides Importance) (int, error) {
	entry, err := findRule(id)
	if err != nil {
		return 0, err
	}
	if entry.Category == proseCategory {
		return MaximumImportance, nil
	}
	if fixed, configured := overrides[id]; configured {
		return fixed, nil
	}
	if reported < MinimumImportance || reported > MaximumImportance {
		return 0, fmt.Errorf("review rule %q reported importance must be between 1 and %d", id, MaximumImportance)
	}
	return reported, nil
}

// Prompt requires overrides previously accepted by ValidateImportance.
func Prompt(overrides Importance) string {
	var builder strings.Builder
	builder.WriteString(strings.TrimSpace(rules.Preamble))
	builder.WriteString("\nReturn each finding with the exact rule_id from this catalog. Use the most specific applicable rule. Apply each fixed importance before the publication threshold. Prose findings always have importance 10 regardless of technical impact.\n")
	for _, entry := range rules.Rules {
		fmt.Fprintf(&builder, "\n## %s [%s]\n", entry.Title, entry.ID)
		if entry.Category == proseCategory {
			fmt.Fprintf(&builder, "Fixed importance: %d.\n", MaximumImportance)
		} else if fixed, configured := overrides[entry.ID]; configured {
			fmt.Fprintf(&builder, "Fixed importance: %d.\n", fixed)
		} else {
			builder.WriteString("Assign importance from the verified impact.\n")
		}
		builder.WriteString(entry.Instructions)
		builder.WriteByte('\n')
	}
	return builder.String()
}

func findRule(id ID) (rule, error) {
	if catalogError != nil {
		return rule{}, catalogError
	}
	for _, entry := range rules.Rules {
		if entry.ID == id {
			return entry, nil
		}
	}
	return rule{}, fmt.Errorf("unknown review rule ID %q", id)
}
