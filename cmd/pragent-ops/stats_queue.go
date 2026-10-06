package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"time"

	"goodkind.io/pr-review-agent/internal/clock"
	"goodkind.io/pr-review-agent/internal/cloudflareops"
	"goodkind.io/pr-review-agent/internal/opsstats"
)

func statsQueue(ctx context.Context, runtimeData []byte, tokenFile string) opsstats.QueueSnapshot {
	var snapshot opsstats.QueueSnapshot
	snapshot.CapturedAt = clock.System().UTC()
	var runtime struct {
		URL string `json:"PROVIDER_BUDGET_URL"`
	}
	if json.Unmarshal(runtimeData, &runtime) != nil {
		snapshot.UnavailableReason = "The runtime queue origin could not be decoded."
		return snapshot
	}
	endpoint, err := url.Parse(runtime.URL)
	if err != nil || endpoint.Scheme != "https" || endpoint.Host == "" || endpoint.User != nil {
		snapshot.UnavailableReason = "The runtime queue origin requires an HTTPS URL."
		return snapshot
	}
	endpoint.Path = "/internal/v1/reassessments"
	endpoint.RawPath, endpoint.RawQuery, endpoint.Fragment = "", "", ""
	token, err := cloudflareops.ReadCredential(tokenFile)
	if err != nil {
		snapshot.UnavailableReason = "The operator credential file could not be read."
		return snapshot
	}
	body, err := json.Marshal(struct {
		Action string `json:"action"`
		Key    string `json:"key"`
	}{Action: "query"})
	if err != nil {
		snapshot.UnavailableReason = "The queue query could not be encoded."
		return snapshot
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		snapshot.UnavailableReason = "The queue query could not be constructed."
		return snapshot
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+string(token))
	client := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
		return errors.New("queue queries do not follow redirects")
	}}
	response, err := client.Do(request)
	if err != nil {
		snapshot.UnavailableReason = "The queue query failed."
		return snapshot
	}
	defer response.Body.Close()
	snapshot.HTTPStatus = response.StatusCode
	if response.StatusCode != http.StatusOK {
		snapshot.UnavailableReason = "The queue endpoint did not return a current snapshot."
		return snapshot
	}
	data, err := io.ReadAll(response.Body)
	if err != nil {
		snapshot.UnavailableReason = "The queue response could not be read."
		return snapshot
	}
	var payload struct {
		Records json.RawMessage `json:"records"`
	}
	if json.Unmarshal(data, &payload) != nil || len(payload.Records) == 0 {
		snapshot.UnavailableReason = "The queue response does not contain a records array."
		return snapshot
	}
	if json.Unmarshal(payload.Records, &snapshot.Records) != nil {
		snapshot.UnavailableReason = "The queue records could not be decoded."
		return snapshot
	}
	snapshot.Known = true
	return snapshot
}
