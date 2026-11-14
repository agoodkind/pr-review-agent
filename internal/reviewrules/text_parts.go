package reviewrules

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

var (
	errTextPartsArray = errors.New("instruction text requires an array of strings")
	errTextPartString = errors.New("instruction part must be a string")
)

// TextParts stores ordered instruction lines.
//
// Empty entries preserve blank lines when the policy assembles the text.
//
// For example:
//
//	parts := TextParts{"First instruction.", "", "Second instruction."}
type TextParts []string

// UnmarshalJSON decodes an array of instruction strings.
//
// The decoder rejects scalar text and null entries.
//
// For example:
//
//	err := json.Unmarshal(data, &parts)
func (parts *TextParts) UnmarshalJSON(data []byte) error {
	var encoded []json.RawMessage
	if err := json.Unmarshal(data, &encoded); err != nil {
		return fmt.Errorf("decode instruction text: %w", err)
	}

	if encoded == nil {
		return errTextPartsArray
	}

	decoded := make(TextParts, len(encoded))
	for index, entry := range encoded {
		var value *string
		if err := json.Unmarshal(entry, &value); err != nil {
			return fmt.Errorf("decode instruction part %d: %w", index, err)
		}

		if value == nil {
			return fmt.Errorf("instruction part %d: %w", index, errTextPartString)
		}

		decoded[index] = *value
	}

	*parts = decoded

	return nil
}

func (parts TextParts) text() string {
	return strings.Join(parts, "\n")
}
