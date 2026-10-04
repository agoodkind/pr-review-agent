// Package quota calculates accounting decisions in Go; the Worker stores usage.
package quota

import (
	"cmp"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"
	_ "time/tzdata"

	"github.com/robfig/cron/v3"
)

// MaximumInteger is the largest integer represented exactly by the storage bridge.
const MaximumInteger int64 = 1<<53 - 1

// TokenType counts cached input and reasoning output within their parent categories.
type TokenType string

const (
	// InputTokens counts all input tokens, including cached input.
	InputTokens TokenType = "input"
	// OutputTokens counts all output tokens, including reasoning.
	OutputTokens TokenType = "output"
)

// WindowMode distinguishes elapsed-time intervals from calendar resets.
type WindowMode string

const (
	// Rolling excludes usage exactly one duration before the check.
	Rolling WindowMode = "rolling"
	// Fixed intervals retain their anchor across process restarts.
	Fixed WindowMode = "fixed"
	// Cron follows the configured timezone, including daylight-saving changes.
	Cron WindowMode = "cron"
)

// Window uses elapsed-time durations for rolling/fixed modes and calendar time for cron.
type Window struct {
	Mode     WindowMode `json:"mode"`
	Duration string     `json:"duration,omitempty"`
	Anchor   string     `json:"anchor,omitempty"`
	Schedule string     `json:"schedule,omitempty"`
	Timezone string     `json:"timezone,omitempty"`
}

// Bounds records endpoint inclusion because rolling and reset-based windows differ.
type Bounds struct {
	StartMS      int64
	EndMS        int64
	IncludeStart bool
	IncludeEnd   bool
}

// Validate requires millisecond precision at the JavaScript storage boundary.
func (window Window) Validate() error {
	switch window.Mode {
	case Rolling, Fixed:
		duration, err := time.ParseDuration(window.Duration)
		if err != nil || duration <= 0 || duration%time.Millisecond != 0 {
			return errors.New("duration must be a positive whole number of milliseconds")
		}
		if window.Schedule != "" || window.Timezone != "" {
			return errors.New("elapsed-time windows cannot set schedule or timezone")
		}
		if window.Mode == Rolling {
			if window.Anchor != "" {
				return errors.New("rolling windows cannot set anchor")
			}
			return nil
		}
		anchor, err := time.Parse(time.RFC3339Nano, window.Anchor)
		if err != nil || anchor.UnixMilli() < 0 || anchor.Nanosecond()%int(time.Millisecond) != 0 {
			return errors.New("fixed windows require an RFC3339 anchor with millisecond precision or less after the Unix epoch")
		}
		return nil
	case Cron:
		if window.Duration != "" || window.Anchor != "" {
			return errors.New("cron windows cannot set duration or anchor")
		}
		_, err := window.cronSchedule()
		return err
	default:
		return errors.New("window mode must be rolling, fixed, or cron")
	}
}

// Bounds assigns reset timestamps to the new fixed or cron interval.
func (window Window) Bounds(now time.Time) (Bounds, error) {
	if err := window.Validate(); err != nil {
		return Bounds{}, err
	}
	nowMS := now.UnixMilli()
	if nowMS < 0 || nowMS > MaximumInteger {
		return Bounds{}, errors.New("accounting timestamp is outside the storage range")
	}
	if window.Mode == Cron {
		schedule, err := window.cronSchedule()
		if err != nil {
			return Bounds{}, err
		}
		next := schedule.Next(now)
		previous, err := previousOccurrence(schedule, now)
		if err != nil || next.IsZero() {
			return Bounds{}, errors.New("cron schedule has no surrounding accounting interval")
		}
		return Bounds{StartMS: previous.UnixMilli(), EndMS: next.UnixMilli(), IncludeStart: true, IncludeEnd: false}, nil
	}
	duration, _ := time.ParseDuration(window.Duration)
	durationMS := duration.Milliseconds()
	if window.Mode == Rolling {
		return Bounds{StartMS: max(0, nowMS-durationMS), EndMS: nowMS, IncludeStart: false, IncludeEnd: true}, nil
	}
	anchor, _ := time.Parse(time.RFC3339Nano, window.Anchor)
	difference := nowMS - anchor.UnixMilli()
	interval := difference / durationMS
	if difference < 0 && difference%durationMS != 0 {
		interval--
	}
	start := anchor.UnixMilli() + interval*durationMS
	return Bounds{StartMS: max(0, start), EndMS: start + durationMS, IncludeStart: true, IncludeEnd: false}, nil
}

func (window Window) cronSchedule() (cron.Schedule, error) {
	if window.Timezone == "" || window.Timezone == "Local" || len(strings.Fields(window.Schedule)) != 5 {
		return nil, errors.New("cron windows require a five-field schedule and timezone")
	}
	if _, err := time.LoadLocation(window.Timezone); err != nil {
		slog.Warn("invalid cron timezone", slog.String("timezone", window.Timezone), slog.String("err", err.Error()))
		return nil, fmt.Errorf("invalid cron timezone: %w", err)
	}
	parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)
	schedule, err := parser.Parse("CRON_TZ=" + window.Timezone + " " + window.Schedule)
	if err != nil {
		slog.Warn("invalid cron schedule", slog.String("err", err.Error()))
		return nil, fmt.Errorf("parse quota cron schedule: %w", err)
	}
	// The five years after the storage epoch include a leap year.
	if schedule.Next(time.Unix(0, 0).UTC()).IsZero() {
		return nil, errors.New("cron schedule has no possible calendar reset")
	}
	return schedule, nil
}

func previousOccurrence(schedule cron.Schedule, now time.Time) (time.Time, error) {
	upper := now.Unix()
	const maximumLookback = int64(8 * 366 * 24 * 60 * 60)
	for distance := int64(60); distance <= maximumLookback; distance *= 2 {
		lower := max(0, upper-distance)
		next := schedule.Next(time.Unix(lower, 0))
		if !next.IsZero() && !next.After(now) {
			for upper-lower > 1 {
				middle := lower + (upper-lower)/2
				candidate := schedule.Next(time.Unix(middle, 0))
				if !candidate.IsZero() && !candidate.After(now) {
					lower = middle
				} else {
					upper = middle
				}
			}
			return schedule.Next(time.Unix(lower, 0)), nil
		}
		if lower == 0 {
			break
		}
	}
	return time.Time{}, errors.New("cron schedule has no previous reset within eight years")
}

// Usage stores the provider total independently of selected input/output categories.
type Usage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	TotalTokens  int64 `json:"total_tokens"`
}

// Tokens uses the provider total when the configured category list is empty.
func (usage Usage) Tokens(types []TokenType) int64 {
	if len(types) == 0 {
		return usage.TotalTokens
	}
	var tokens int64
	for _, tokenType := range types {
		switch tokenType {
		case InputTokens:
			tokens += usage.InputTokens
		case OutputTokens:
			tokens += usage.OutputTokens
		}
	}
	return tokens
}

// Event attributes usage to admission even when the response finishes in a later window.
type Event struct {
	Sequence     int64 `json:"sequence"`
	AdmittedAtMS int64 `json:"admitted_at_ms"`
	Usage
}

// Snapshot separates measured-history coverage from admission permission.
type Snapshot struct {
	Allowed         bool
	Used            int64
	Limit           int64
	Remaining       int64
	AvailableAtMS   int64
	HistoryStartMS  int64
	HistoryComplete bool
	Bounds          Bounds
}

// Evaluate checks reported usage without reserving tokens for requests still running.
func Evaluate(window Window, now time.Time, limit int64, types []TokenType, events []Event, historyStartMS int64) (Snapshot, error) {
	bounds, err := window.Bounds(now)
	if err != nil {
		return Snapshot{}, err
	}
	snapshot := Snapshot{
		Allowed: false, Used: 0, Limit: limit, Remaining: 0, AvailableAtMS: 0, Bounds: bounds, HistoryStartMS: historyStartMS,
		HistoryComplete: historyStartMS > 0 && bounds.StartMS >= historyStartMS,
	}
	selected := make([]Event, 0, len(events))
	for _, event := range events {
		if !bounds.contains(event.AdmittedAtMS) {
			continue
		}
		tokens := event.Tokens(types)
		if tokens < 0 || tokens > MaximumInteger || snapshot.Used > MaximumInteger-tokens {
			return Snapshot{}, errors.New("reported usage exceeds the accounting integer range")
		}
		snapshot.Used += tokens
		selected = append(selected, event)
	}
	snapshot.Allowed = limit == 0 || snapshot.Used < limit
	snapshot.Remaining = max(0, limit-snapshot.Used)
	if snapshot.Allowed {
		return snapshot, nil
	}
	if window.Mode != Rolling {
		snapshot.AvailableAtMS = bounds.EndMS
		return snapshot, nil
	}
	snapshot.AvailableAtMS = rollingAvailableAt(window, selected, types, snapshot.Used, limit)
	return snapshot, nil
}

func rollingAvailableAt(window Window, events []Event, types []TokenType, used, limit int64) int64 {
	slices.SortFunc(events, func(left, right Event) int { return cmp.Compare(left.AdmittedAtMS, right.AdmittedAtMS) })
	duration, _ := time.ParseDuration(window.Duration)
	for _, event := range events {
		used -= event.Tokens(types)
		if used < limit {
			return event.AdmittedAtMS + duration.Milliseconds()
		}
	}
	return 0
}

func (bounds Bounds) contains(timestamp int64) bool {
	if timestamp < bounds.StartMS || timestamp > bounds.EndMS {
		return false
	}
	return (timestamp != bounds.StartMS || bounds.IncludeStart) && (timestamp != bounds.EndMS || bounds.IncludeEnd)
}
