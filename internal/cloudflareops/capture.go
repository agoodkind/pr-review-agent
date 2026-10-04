package cloudflareops

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Event retains raw source fields because historical forwarding converts numbers and booleans to strings.
type Event struct {
	Timestamp int64                      `json:"timestamp"`
	Source    map[string]json.RawMessage `json:"source"`
	Metadata  map[string]json.RawMessage `json:"$metadata"`
	Workers   map[string]json.RawMessage `json:"$workers"`
	Dataset   string                     `json:"dataset,omitempty"`
}

func text(fields map[string]json.RawMessage, key string) string {
	var value string
	_ = json.Unmarshal(fields[key], &value)
	return value
}

// Legacy log forwarding stores numeric and boolean fields as JSON strings.
func number(fields map[string]json.RawMessage, key string) (int64, bool) {
	raw := fields[key]
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return 0, false
	}
	var value int64
	if json.Unmarshal(raw, &value) == nil {
		return value, true
	}
	parsed, err := strconv.ParseInt(text(fields, key), 10, 64)
	return parsed, err == nil
}

func boolean(fields map[string]json.RawMessage, key string) bool {
	value, _ := booleanValue(fields, key)
	return value
}

func booleanValue(fields map[string]json.RawMessage, key string) (bool, bool) {
	raw := fields[key]
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return false, false
	}
	var value bool
	if json.Unmarshal(raw, &value) == nil {
		return value, true
	}
	encoded := text(fields, key)
	if encoded == "true" {
		return true, true
	}
	if encoded == "false" {
		return false, true
	}
	return false, false
}

func (event Event) script() string {
	if script := text(event.Workers, "scriptName"); script != "" {
		return script
	}
	return text(event.Metadata, "service")
}

func (event Event) identity() (string, error) {
	id := text(event.Metadata, "id")
	if id == "" {
		encoded, err := json.Marshal(event)
		if err != nil {
			return "", errors.New("event identity cannot be encoded")
		}
		digest := sha256.Sum256(encoded)
		id = hex.EncodeToString(digest[:])
	}
	return strconv.FormatInt(event.Timestamp, 10) + ":" + id, nil
}

type eventCollection struct {
	Events *[]Event `json:"events"`
	Count  *int     `json:"count"`
}
type eventResult struct {
	Events eventCollection `json:"events"`
	Run    struct {
		Status string `json:"status"`
	} `json:"run"`
	Statistics map[string]json.RawMessage `json:"statistics"`
}

// CaptureOptions supplies a page limit that cannot certify query exhaustion.
type CaptureOptions struct {
	Account   string
	Script    string
	From      time.Time
	To        time.Time
	PageSize  int
	MaxPages  int
	Directory string
}

// Manifest separates telemetry exhaustion from unverified retention and sampling.
type Manifest struct {
	Directory         string     `json:"directory"`
	From              time.Time  `json:"from"`
	To                time.Time  `json:"to"`
	Account           string     `json:"account,omitempty"`
	Script            string     `json:"script,omitempty"`
	Records           int        `json:"records"`
	Pages             int        `json:"pages,omitempty"`
	Duplicates        int        `json:"duplicates,omitempty"`
	Excluded          int        `json:"excluded_script_records,omitempty"`
	Complete          bool       `json:"complete"`
	Completeness      string     `json:"completeness"`
	ObservedFrom      *time.Time `json:"observed_from,omitempty"`
	ObservedTo        *time.Time `json:"observed_to,omitempty"`
	CredentialRevoked bool       `json:"credential_revoked,omitempty"`
	EventsFile        string     `json:"events_file,omitempty"`
	InputFile         string     `json:"input_file,omitempty"`
	Error             string     `json:"error,omitempty"`
}

// PrivateDirectory refuses existing destinations to prevent overwriting retained evidence.
func PrivateDirectory(path string) (string, error) {
	if path == "" {
		directory, err := os.MkdirTemp("", "pragent-ops.")
		if err != nil {
			slog.Warn("Private output directory creation failed")
			return "", fmt.Errorf("create temporary output directory: %w", err)
		}
		return directory, nil
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		slog.Warn("Output directory creation failed", "path", path)
		return "", fmt.Errorf("create output directory: %w", err)
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		slog.Warn("Output directory resolution failed", "path", path)
		return "", fmt.Errorf("resolve output directory: %w", err)
	}
	return absolute, nil
}

// WriteJSON sets private permissions because telemetry evidence can contain provider error payloads.
func WriteJSON(path string, value json.Marshaler) error {
	slog.Info("Private evidence artifact write started", "path", path)
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return errors.New("output could not be encoded")
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		slog.Warn("Private artifact write failed", "path", path)
		return fmt.Errorf("write private JSON artifact: %w", err)
	}
	return nil
}

// MarshalJSON uses a distinct wire type because encoding Manifest directly would recurse.
func (manifest Manifest) MarshalJSON() ([]byte, error) {
	type wireManifest Manifest
	data, err := json.Marshal(wireManifest(manifest))
	if err != nil {
		return nil, fmt.Errorf("encode capture manifest: %w", err)
	}
	return data, nil
}

// Capture requires an empty terminal page because a short page does not establish query exhaustion.
func (client *Client) Capture(ctx context.Context, options CaptureOptions) (manifest Manifest, returnErr error) {
	slog.InfoContext(ctx, "Telemetry capture started", "account", options.Account, "script", options.Script)
	manifest.Directory = options.Directory
	manifest.From = options.From.UTC()
	manifest.To = options.To.UTC()
	manifest.Account = options.Account
	manifest.Script = options.Script
	manifest.Completeness = "incomplete"
	if options.Script == "" || !options.From.Before(options.To) || options.PageSize <= 0 || options.PageSize > 2000 || options.MaxPages <= 0 {
		return manifest, errors.New("capture requires a script, increasing time range, page size 1..2000, and positive page limit")
	}
	manifest.EventsFile = filepath.Join(options.Directory, "events.ndjson")
	file, err := os.OpenFile(manifest.EventsFile, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		slog.WarnContext(ctx, "Telemetry output file creation failed")
		return manifest, fmt.Errorf("create telemetry output file: %w", err)
	}
	writer := bufio.NewWriter(file)
	defer func() {
		returnErr = errors.Join(returnErr, writer.Flush(), file.Close())
		if returnErr != nil {
			manifest.Complete = false
			manifest.Completeness = "incomplete"
			manifest.Error = "capture failed; saved records are partial"
		}
		returnErr = errors.Join(returnErr, WriteJSON(filepath.Join(options.Directory, "manifest.json"), manifest))
	}()
	parameters, err := json.Marshal(Parameters{Datasets: []string{"cloudflare-workers"}, FilterCombination: "and", Filters: []Filter{{Key: "$metadata.service", Operation: "includes", Type: "string", Value: options.Script}}})
	if err != nil {
		return manifest, errors.New("telemetry parameters could not be encoded")
	}
	collector := captureCollector{options: options, manifest: &manifest, encoder: json.NewEncoder(writer), seen: make(map[string]struct{})}
	upper := options.To.UnixMilli()
	for page := 1; page <= options.MaxPages; page++ {
		if err := ctx.Err(); err != nil {
			return manifest, fmt.Errorf("telemetry capture canceled: %w", err)
		}
		result, err := client.Query(ctx, options.Account, QueryRequest{QueryID: "pragent-ops-capture", Timeframe: Timeframe{From: options.From.UnixMilli(), To: upper}, Parameters: parameters, Limit: options.PageSize, View: "events"})
		if err != nil {
			return manifest, err
		}
		if err = os.WriteFile(filepath.Join(options.Directory, fmt.Sprintf("page-%04d.json", page)), result, 0o600); err != nil {
			slog.WarnContext(ctx, "Telemetry page recording failed", "page", page)
			return manifest, fmt.Errorf("write telemetry page: %w", err)
		}
		manifest.Pages = page
		events, err := ReadQueryEvents(result)
		if err != nil {
			return manifest, err
		}
		if len(events) == 0 {
			manifest.Complete = true
			manifest.Completeness = "available telemetry exhausted; retention and sampling are not proven"
			return manifest, nil
		}
		oldest, newRecords, err := collector.page(events, upper)
		if err != nil {
			return manifest, err
		}
		if oldest >= upper || newRecords == 0 {
			return manifest, errors.New("cloudflare pagination did not advance; saved results are partial")
		}
		upper = oldest
	}
	return manifest, errors.New("cloudflare page safety limit exceeded; saved results are partial")
}

// ReadQueryEvents requires the event array because a missing field cannot prove zero results.
func ReadQueryEvents(result json.RawMessage) ([]Event, error) {
	var decoded eventResult
	if json.Unmarshal(result, &decoded) != nil || decoded.Events.Events == nil {
		return nil, errors.New("cloudflare query omitted a valid event array")
	}
	if boolean(decoded.Statistics, "truncated") || boolean(decoded.Statistics, "incomplete") || (decoded.Run.Status != "" && strings.ToUpper(decoded.Run.Status) != "COMPLETED") {
		return nil, errors.New("cloudflare returned an incomplete query result")
	}
	return *decoded.Events.Events, nil
}

type captureCollector struct {
	options  CaptureOptions
	manifest *Manifest
	encoder  *json.Encoder
	seen     map[string]struct{}
}

func (collector *captureCollector) page(events []Event, upper int64) (int64, int, error) {
	oldest := upper
	newRecords := 0
	for _, event := range events {
		if event.Timestamp <= 0 || event.Timestamp < collector.options.From.UnixMilli() || event.Timestamp > upper {
			return oldest, newRecords, errors.New("cloudflare returned an event outside the query range")
		}
		if boolean(event.Workers, "truncated") {
			return oldest, newRecords, errors.New("cloudflare returned a truncated log record")
		}
		oldest = min(oldest, event.Timestamp)
		identity, err := event.identity()
		if err != nil {
			return oldest, newRecords, err
		}
		if _, exists := collector.seen[identity]; exists {
			collector.manifest.Duplicates++
			continue
		}
		collector.seen[identity] = struct{}{}
		newRecords++
		if event.script() == "" {
			return oldest, newRecords, errors.New("cloudflare event omitted script metadata")
		}
		if event.script() != collector.options.Script {
			collector.manifest.Excluded++
			continue
		}
		if err := collector.encoder.Encode(event); err != nil {
			slog.Warn("Telemetry record encoding failed")
			return oldest, newRecords, fmt.Errorf("encode telemetry record: %w", err)
		}
		collector.manifest.Records++
		observed := time.UnixMilli(event.Timestamp).UTC()
		if collector.manifest.ObservedFrom == nil || observed.Before(*collector.manifest.ObservedFrom) {
			collector.manifest.ObservedFrom = &observed
		}
		if collector.manifest.ObservedTo == nil || observed.After(*collector.manifest.ObservedTo) {
			collector.manifest.ObservedTo = &observed
		}
	}
	return oldest, newRecords, nil
}

// ReadEvents preserves support for bare arrays from earlier captures.
func ReadEvents(path string) ([]Event, error) {
	file, err := os.Open(path)
	if err != nil {
		slog.Warn("Telemetry input read failed", "path", path)
		return nil, fmt.Errorf("open event input: %w", err)
	}
	defer func() { _ = file.Close() }()
	reader := bufio.NewReader(file)
	first, err := reader.Peek(1)
	if errors.Is(err, io.EOF) {
		return []Event{}, nil
	}
	if err != nil {
		return nil, errors.New("event input is empty")
	}
	if first[0] == '[' {
		var events []Event
		if err := json.NewDecoder(reader).Decode(&events); err != nil {
			return nil, errors.New("event array is invalid")
		}
		if events == nil {
			return nil, errors.New("event array is null")
		}
		return events, nil
	}
	var events []Event
	decoder := json.NewDecoder(reader)
	for {
		var event Event
		err := decoder.Decode(&event)
		if errors.Is(err, io.EOF) {
			return events, nil
		}
		if err != nil {
			return nil, errors.New("event NDJSON is invalid")
		}
		events = append(events, event)
	}
}

// LoadManifest reads the original interval before cached metrics can narrow the range.
func LoadManifest(path string) (Manifest, error) {
	var manifest Manifest
	data, err := os.ReadFile(path)
	if err != nil {
		slog.Warn("Capture manifest read failed", "path", path)
		return manifest, fmt.Errorf("read capture manifest: %w", err)
	}
	if json.Unmarshal(bytes.TrimSpace(data), &manifest) != nil {
		return manifest, errors.New("capture manifest is invalid")
	}
	return manifest, nil
}
