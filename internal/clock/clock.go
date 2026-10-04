// Package clock isolates operating-system time reads from runtime calculations.
package clock

import "time"

// Clock separates current-time acquisition from admission timestamp attribution.
type Clock func() time.Time

// System supplies an instant; accounting windows select their own timezones.
func System() time.Time {
	return time.Now()
}
