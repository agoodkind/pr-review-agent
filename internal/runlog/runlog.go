// Package runlog keeps one review's own log so the check run can show it.
//
// A failed review used to report only the stage that failed, for example
// "Review failed during model analysis." The reader could not tell which chunk
// was slow, which model answered, or how the time was spent, because the
// service's logs never left the container. A Recorder captures every log line
// the review already writes and renders them for the check run body.
package runlog

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"
)

// MaximumRecords bounds what one run retains. A review that logs more than this
// keeps the newest lines, because the end of a run holds the failure.
const MaximumRecords = 500

// Entry is one captured log line.
type Entry struct {
	Time    time.Time
	Level   slog.Level
	Message string
	Fields  map[string]string
}

// Recorder captures log records for a single review run. It is an
// [slog.Handler], so it records what the review already logs and needs no new
// call sites.
type Recorder struct {
	attributes []slog.Attr

	mu      sync.Mutex
	entries *[]Entry
}

// NewRecorder returns a recorder ready to wrap a logger for one run.
func NewRecorder() *Recorder {
	entries := make([]Entry, 0, MaximumRecords)
	return &Recorder{
		attributes: nil,
		mu:         sync.Mutex{},
		entries:    &entries,
	}
}

// Enabled records every level, because a failure explains itself with the
// detail that led to it.
func (recorder *Recorder) Enabled(context.Context, slog.Level) bool {
	return recorder != nil
}

// Handle captures one record, dropping the oldest when the buffer is full.
func (recorder *Recorder) Handle(_ context.Context, record slog.Record) error {
	if recorder == nil {
		return nil
	}
	fields := make(map[string]string, record.NumAttrs()+len(recorder.attributes))
	for _, attribute := range recorder.attributes {
		fields[attribute.Key] = attribute.Value.String()
	}
	record.Attrs(func(attribute slog.Attr) bool {
		fields[attribute.Key] = attribute.Value.String()
		return true
	})

	entry := Entry{
		Time:    record.Time,
		Level:   record.Level,
		Message: record.Message,
		Fields:  fields,
	}

	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	captured := *recorder.entries
	if len(captured) >= MaximumRecords {
		captured = captured[1:]
	}
	captured = append(captured, entry)
	*recorder.entries = captured
	return nil
}

// WithAttrs returns a recorder that adds the given attributes to every entry
// and writes into the same buffer.
func (recorder *Recorder) WithAttrs(attrs []slog.Attr) slog.Handler {
	if recorder == nil {
		return nil
	}
	combined := make([]slog.Attr, 0, len(recorder.attributes)+len(attrs))
	combined = append(combined, recorder.attributes...)
	combined = append(combined, attrs...)
	return &Recorder{
		attributes: combined,
		mu:         sync.Mutex{},
		entries:    recorder.entries,
	}
}

// WithGroup returns the recorder unchanged, because captured entries are flat.
func (recorder *Recorder) WithGroup(string) slog.Handler {
	return recorder
}

// Entries returns what the run logged, oldest first.
func (recorder *Recorder) Entries() []Entry {
	if recorder == nil {
		return nil
	}
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	captured := *recorder.entries
	copied := make([]Entry, len(captured))
	copy(copied, captured)
	return copied
}

// publishedFields are the field names whose values may reach a public surface.
// Every one of them holds a measurement this run counted, an identifier a
// reader can look up, or a phrase this service itself wrote.
//
// This is an allowlist, and the direction is the whole point. A check run is as
// public and as permanent as a pull request comment, and a model provider error
// can carry the request it failed on, an internal endpoint, or a credential.
// Naming what is safe means a field someone adds later and forgets to list here
// is withheld rather than published: forgetting costs a reader one lookup in
// this service's log, where publishing a credential costs something nobody can
// take back.
//
// A withheld field keeps its key, its line's message, and every other field on
// that line, so a reader still sees which line carried it. Entries keeps the
// value itself, so the service log the run identifier points at still holds it.
var publishedFields = map[string]struct{}{
	// Which run this was, and what it ran against.
	"delivery_id":  {},
	"run_id":       {},
	"repository":   {},
	"pull_request": {},
	"head":         {},
	"check_run_id": {},
	"comment_id":   {},
	"review_id":    {},

	// How the run was configured, and what it concluded.
	"minimum_importance": {},
	"chunk_timeout":      {},
	"status":             {},
	"conclusion":         {},
	"decision":           {},
	"event":              {},
	"visible":            {},
	// reason and blocking carry sentences this service composed: why a run was
	// suppressed or declined, and what an open thread is holding.
	"reason":   {},
	"blocking": {},

	// How much of the delta there was, and how much of it was read.
	"files":             {},
	"chunk":             {},
	"chunks":            {},
	"delta_chunks":      {},
	"already_read":      {},
	"owed":              {},
	"pending":           {},
	"concurrency":       {},
	"chunks_failed":     {},
	"coverage_complete": {},
	"hunks":             {},
	"path":              {},
	"paths":             {},
	"line":              {},

	// What the model was asked, and how the request went. The model name is the
	// service's own configuration, and the check run already reports it.
	"model":          {},
	"model_requests": {},
	"elapsed":        {},
	"findings":       {},
	"prompt_bytes":   {},
	"truncated":      {},
	"attempt":        {},
	"attempts":       {},
	"recovered":      {},

	// What reached the pull request. The finding and thread traces carry an
	// identifier, a path, a line range, and an importance, and no model prose.
	"eligible":             {},
	"published":            {},
	"offered":              {},
	"posted":               {},
	"refused":              {},
	"failed":               {},
	"comments_posted":      {},
	"comments_rejected":    {},
	"comments_undelivered": {},
	"streamed_comments":    {},
	"observed_findings":    {},
	"eligible_findings":    {},
	"published_findings":   {},
	"bot_reviews":          {},
	"bot_threads":          {},
}

// withheldValue stands in for a dropped value and says where the real one is.
const withheldValue = "[redacted: see this service's log for this run]"

// Render returns the run's log as a markdown block for a check run body, with
// the value of every field this service has not vouched for withheld. It
// returns the empty string when the run logged nothing, so a caller can omit
// the section rather than publish an empty heading.
func (recorder *Recorder) Render() string {
	entries := recorder.Entries()
	if len(entries) == 0 {
		return ""
	}
	var builder strings.Builder
	builder.WriteString("## Run log\n\n```\n")
	for _, entry := range entries {
		builder.WriteString(formatEntry(entry))
		builder.WriteString("\n")
	}
	builder.WriteString("```\n")
	return builder.String()
}

func formatEntry(entry Entry) string {
	stamp := entry.Time.UTC().Format("15:04:05.000")
	line := fmt.Sprintf("%s %-5s %s", stamp, entry.Level.String(), entry.Message)
	if len(entry.Fields) == 0 {
		return line
	}
	return line + " " + formatFields(entry.Fields)
}

func formatFields(fields map[string]string) string {
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, key+"="+publishedValue(key, fields[key]))
	}
	return strings.Join(parts, " ")
}

// lineBreakEscaper keeps a published value on its own line.
//
// Allowlisting a field says its value is safe to read, not that its shape is.
// A repository path comes from the pull request, so someone who can name a file
// can put a newline in it, and a raw newline here would close the code block
// this log renders into and let the rest of that value become markdown in a
// body the service signed its name to.
var lineBreakEscaper = strings.NewReplacer("\r", "\\r", "\n", "\\n")

// EscapeLineBreaks keeps one repository controlled value on the line it was
// rendered onto.
//
// It is exported because the run log is not the only place this service prints
// a path someone else chose inside a code block it signs its name to. Every
// such surface has the same problem and takes the same answer, so they share
// this one rather than each growing an escape of its own.
func EscapeLineBreaks(value string) string {
	return lineBreakEscaper.Replace(value)
}

// publishedValue returns what one field may say on a public surface.
func publishedValue(key string, value string) string {
	if _, published := publishedFields[key]; published {
		return EscapeLineBreaks(value)
	}
	return withheldValue
}

// Tee returns a handler that writes to every handler given. It lets one review
// keep its own log while the service keeps writing to stdout.
func Tee(handlers ...slog.Handler) slog.Handler {
	present := make([]slog.Handler, 0, len(handlers))
	for _, handler := range handlers {
		if handler != nil {
			present = append(present, handler)
		}
	}
	return &teeHandler{handlers: present}
}

type teeHandler struct {
	handlers []slog.Handler
}

func (tee *teeHandler) Enabled(ctx context.Context, level slog.Level) bool {
	for _, handler := range tee.handlers {
		if handler.Enabled(ctx, level) {
			return true
		}
	}
	return false
}

// Handle writes the record to every sink and never fails. One sink refusing a
// line must not stop the others, and a log handler cannot report its own
// failure through slog without recursing back into itself.
func (tee *teeHandler) Handle(ctx context.Context, record slog.Record) error {
	for _, handler := range tee.handlers {
		if !handler.Enabled(ctx, record.Level) {
			continue
		}
		_ = handler.Handle(ctx, record.Clone())
	}
	return nil
}

func (tee *teeHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	next := make([]slog.Handler, 0, len(tee.handlers))
	for _, handler := range tee.handlers {
		next = append(next, handler.WithAttrs(attrs))
	}
	return &teeHandler{handlers: next}
}

func (tee *teeHandler) WithGroup(name string) slog.Handler {
	next := make([]slog.Handler, 0, len(tee.handlers))
	for _, handler := range tee.handlers {
		next = append(next, handler.WithGroup(name))
	}
	return &teeHandler{handlers: next}
}
