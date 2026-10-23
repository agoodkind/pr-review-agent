package cloudflareops

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"time"

	"goodkind.io/pr-review-agent/internal/clock"
)

var deploymentTestNamePattern = regexp.MustCompile(`^pragent-deploy-test-[a-f0-9]{8}-gateway$`)

// DeploymentTestOptions requires an isolated gateway and independently built images.
type DeploymentTestOptions struct {
	Deployment       DeploymentOptions
	BaselineImage    string
	BaselineRevision string
	BaselineWrangler string
}

type testContainerApplication struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Configuration struct {
		Image string `json:"image"`
	} `json:"configuration"`
}

type testDeploymentSnapshot struct {
	Selection          deploymentSelection            `json:"selection"`
	Revision           string                         `json:"revision"`
	Bindings           map[string]testBindingMetadata `json:"bindings"`
	Applications       []testContainerApplication     `json:"applications"`
	WorkerDeploymentID string                         `json:"worker_deployment_id"`
	WorkerVersionID    string                         `json:"worker_version_id"`
}

type testBindingMetadata struct {
	Type        workerBindingType `json:"type"`
	NamespaceID string            `json:"namespace_id,omitempty"`
	ClassName   string            `json:"class_name,omitempty"`
	Service     string            `json:"service,omitempty"`
	Entrypoint  string            `json:"entrypoint,omitempty"`
	Environment string            `json:"environment,omitempty"`
	ValueSHA256 string            `json:"value_sha256,omitempty"`
}

// DeploymentTestResult records actual Cloudflare resources and readiness evidence.
type DeploymentTestResult struct {
	Gateway                   string                 `json:"gateway"`
	Slots                     []DeploymentSlot       `json:"slots"`
	Directory                 string                 `json:"directory"`
	Before                    testDeploymentSnapshot `json:"before"`
	AfterFailure              testDeploymentSnapshot `json:"after_failure"`
	AfterPromotion            testDeploymentSnapshot `json:"after_promotion"`
	Samples                   int                    `json:"samples"`
	FailedSamples             int                    `json:"failed_samples"`
	CleanupComplete           bool                   `json:"cleanup_complete"`
	CredentialRevoked         bool                   `json:"credential_revoked"`
	RejectedCandidateRevision string                 `json:"rejected_candidate_revision,omitempty"`
	PromotionSamples          testAvailabilityCounts `json:"promotion_samples"`
	SecondPromotionSamples    testAvailabilityCounts `json:"second_promotion_samples"`
	AfterSecondPromotion      testDeploymentSnapshot `json:"after_second_promotion"`
	StoppedPreviousStatus     int                    `json:"stopped_previous_status"`
	StoppedPreviousRunning    bool                   `json:"stopped_previous_running"`
}

type testAvailabilityCounts struct {
	Count  int `json:"count"`
	Failed int `json:"failed"`
}

type observedTestDeployment struct {
	deployment      DeploymentResult
	deploymentError error
	samples         testAvailabilityCounts
}

// MarshalJSON writes resource identifiers and generated test configuration privately.
func (result DeploymentTestResult) MarshalJSON() ([]byte, error) {
	type wireResult DeploymentTestResult
	data, err := json.Marshal(wireResult(result))
	if err != nil {
		return nil, fmt.Errorf("encode deployment test result: %w", err)
	}
	return data, nil
}

// TestDeployment runs the real bootstrap, rejected-candidate, and promotion operations.
func TestDeployment(ctx context.Context, options DeploymentTestOptions) (result DeploymentTestResult, returnErr error) {
	slog.InfoContext(ctx, "Isolated deployment test started")
	operation, err := prepareDeployment(options.Deployment)
	if err != nil {
		return result, err
	}
	if err := operation.requireIsolatedTestbed(options); err != nil {
		return result, err
	}
	operation.directory, err = PrivateDirectory(options.Deployment.Directory)
	if err != nil {
		return result, err
	}
	result.Gateway, result.Slots, result.Directory = operation.name, operation.configuration.Slots, operation.directory
	credentials, err := AcquireCredentials(ctx, CredentialOptions{TokenFile: options.Deployment.TokenFile, AccountTokenFile: options.Deployment.AccountTokenFile, Account: operation.account, TTL: options.Deployment.CredentialTTL, APIURL: APIURL, PermissionNames: []string{"Workers Scripts Write", "Workers Containers Write"}})
	if err != nil {
		return result, err
	}
	operation.client = credentials.Client
	cleanupAllowed := false
	defer func() {
		var diagnosticErr error
		if returnErr != nil && cleanupAllowed {
			diagnosticErr = captureTestFailureReadiness(ctx, operation)
		}
		var cleanupErr error
		if cleanupAllowed {
			cleanupErr = operation.cleanupTestbed(context.WithoutCancel(ctx))
		}
		result.CleanupComplete = cleanupErr == nil
		returnErr = errors.Join(returnErr, diagnosticErr, cleanupErr, credentials.Close(ctx))
		result.CredentialRevoked = credentials.Revoked
		returnErr = errors.Join(returnErr, WriteJSON(filepath.Join(operation.directory, "result.json"), result))
	}()
	if err := operation.requireFreshTestWorkers(ctx); err != nil {
		return result, err
	}
	applications, err := operation.testApplications(ctx)
	if err != nil {
		return result, err
	}
	if len(applications) != 0 {
		return result, errors.New("test container application prefix already exists; refusing to modify it")
	}
	cleanupAllowed = true
	if err := WriteJSON(filepath.Join(operation.directory, "resources.json"), result); err != nil {
		return result, err
	}
	returnErr = operation.runTestDeploymentCases(ctx, options, &result)
	return result, returnErr
}

func (operation *deployment) runTestDeploymentCases(ctx context.Context, options DeploymentTestOptions, result *DeploymentTestResult) (returnErr error) {
	slog.InfoContext(ctx, "Isolated legacy bootstrap started", "gateway", operation.name)
	if err := operation.bootstrapTestGateway(ctx, options.BaselineWrangler, options.BaselineImage, options.BaselineRevision); err != nil {
		return err
	}
	var err error
	result.Before, err = operation.testSnapshot(ctx)
	if err != nil {
		return err
	}
	if len(result.Before.Applications) != 1 {
		return errors.New("isolated baseline must create exactly one registered container application")
	}
	tokenPath := filepath.Join(operation.directory, "controller-token")
	if err := os.WriteFile(tokenPath, []byte(operation.client.token), 0o600); err != nil {
		return errors.New("write private controller credential failed")
	}
	defer func() { returnErr = errors.Join(returnErr, os.Remove(tokenPath)) }()
	deploymentOptions := options.Deployment
	deploymentOptions.TokenFile, deploymentOptions.AccountTokenFile = tokenPath, ""
	deploymentOptions.Directory = filepath.Join(operation.directory, "mismatched-candidate")
	deploymentOptions.Revision = strings.Repeat("0", 40)
	result.Samples, result.FailedSamples, err = operation.testRejectedCandidate(ctx, deploymentOptions, options.BaselineRevision)
	if err != nil {
		return err
	}
	result.RejectedCandidateRevision, err = operation.probe(ctx, operation.configuration.Slots[0].Origin, options.Deployment.Revision)
	if err != nil {
		return err
	}
	result.AfterFailure, err = operation.testSnapshot(ctx)
	if err != nil {
		return err
	}
	if err := verifyRejectedCandidate(result.Before, result.AfterFailure); err != nil {
		return err
	}
	deploymentOptions.Revision = options.Deployment.Revision
	deploymentOptions.Directory = filepath.Join(operation.directory, "healthy-candidate")
	promotion, samples, err := operation.testSuccessfulCandidate(ctx, deploymentOptions, []string{options.BaselineRevision, options.Deployment.Revision}, "promotion-availability.ndjson")
	result.PromotionSamples = samples
	if err != nil {
		return err
	}
	if !promotion.Promoted {
		return errors.New("healthy candidate did not promote")
	}
	result.AfterPromotion, err = operation.testSnapshot(ctx)
	if err != nil {
		return err
	}
	if result.AfterPromotion.Selection.Mode != "service" || result.AfterPromotion.Selection.Service != promotion.Candidate {
		return errors.New("test gateway did not select the verified candidate")
	}
	if result.AfterPromotion.Revision != options.Deployment.Revision {
		return errors.New("promoted test gateway did not report the expected revision")
	}
	return operation.testSecondSlot(ctx, deploymentOptions, result)
}

func (operation *deployment) testSecondSlot(ctx context.Context, options DeploymentOptions, result *DeploymentTestResult) error {
	options.Directory = filepath.Join(operation.directory, "second-candidate")
	observed, err := operation.observeTestDeployment(ctx, options, []string{options.Revision}, "second-promotion-availability.ndjson", operation.deployWithInterruptedHandoff)
	result.SecondPromotionSamples = observed.samples
	if err != nil {
		return err
	}
	if observed.deploymentError != nil {
		return observed.deploymentError
	}
	promotion := observed.deployment
	if !promotion.Promoted {
		return errors.New("resumed second candidate did not promote")
	}
	if promotion.Candidate != operation.configuration.Slots[1].Name {
		return errors.New("second deployment did not choose the independent second slot")
	}
	result.AfterSecondPromotion, err = operation.testSnapshot(ctx)
	if err != nil {
		return err
	}
	if result.AfterSecondPromotion.Selection.Service != promotion.Candidate || options.Revision != result.AfterSecondPromotion.Revision {
		return errors.New("second slot did not become the expected active revision")
	}
	first, second := "", ""
	for _, application := range result.AfterSecondPromotion.Applications {
		if application.Name == operation.configuration.Slots[0].Name {
			first = application.ID
		}
		if application.Name == operation.configuration.Slots[1].Name {
			second = application.ID
		}
	}
	if first == "" || second == "" || first == second {
		return errors.New("review slots do not have independent container applications")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, operation.configuration.Slots[0].Origin+"/", nil)
	if err != nil {
		return errors.New("delayed source health request could not be created")
	}
	response, err := operation.healthClient.Do(request)
	if err != nil {
		slog.WarnContext(ctx, "Delayed source health transport failed")
		return fmt.Errorf("delayed source health transport failed: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	result.StoppedPreviousStatus = response.StatusCode
	if response.StatusCode != http.StatusServiceUnavailable {
		return errors.New("delayed source health request did not preserve the handoff fence")
	}
	state, err := operation.drainRequest(ctx, operation.configuration.Slots[0].Origin, "status")
	if err != nil {
		return err
	}
	result.StoppedPreviousRunning = state.containerRunning()
	if result.StoppedPreviousRunning {
		return errors.New("delayed source health request restarted the stopped previous application")
	}
	return nil
}

func (operation *deployment) requireIsolatedTestbed(options DeploymentTestOptions) error {
	if !deploymentTestNamePattern.MatchString(operation.name) || !imageDigestPattern.MatchString(options.BaselineImage) || !revisionPattern.MatchString(options.BaselineRevision) {
		return errors.New("deployment test requires an isolated generated gateway and immutable baseline image/revision")
	}
	prefix := strings.TrimSuffix(operation.name, "-gateway")
	if operation.configuration.Slots[0].Name != prefix+"-service-a" || operation.configuration.Slots[1].Name != prefix+"-service-b" {
		return errors.New("deployment test requires exactly its generated service-a and service-b Workers")
	}
	if options.BaselineRevision == options.Deployment.Revision {
		return errors.New("deployment test requires distinct real baseline and candidate revisions")
	}
	gatewayOrigin, err := url.Parse(operation.configuration.GatewayOrigin)
	if err != nil || !strings.HasPrefix(gatewayOrigin.Hostname(), operation.name+".") {
		return errors.New("isolated gateway origin does not match its generated Worker name")
	}
	domain := strings.TrimPrefix(gatewayOrigin.Hostname(), operation.name+".")
	for _, slot := range operation.configuration.Slots {
		origin, err := url.Parse(slot.Origin)
		if err != nil || origin.Hostname() != slot.Name+"."+domain {
			return errors.New("isolated service origin does not match its generated Worker name")
		}
	}
	return nil
}

func (operation *deployment) requireFreshTestWorkers(ctx context.Context) error {
	names := []string{operation.name, operation.configuration.Slots[0].Name, operation.configuration.Slots[1].Name}
	for _, name := range names {
		_, exists, err := operation.client.workerSettings(ctx, operation.account, name, true)
		if err != nil {
			return err
		}
		if exists {
			return fmt.Errorf("test Worker %s already exists; refusing to replace it", name)
		}
	}
	return nil
}

func (operation *deployment) bootstrapTestGateway(ctx context.Context, template, image, revision string) error {
	configuration, err := readWrangler(template)
	if err != nil {
		return err
	}
	if configuration.stringField("name") != operation.name || configuration.stringField("account_id") != operation.account {
		return errors.New("baseline Worker must use the registered isolated gateway identity")
	}
	if err := validateTestBaselineBindings(configuration); err != nil {
		return err
	}
	var containers []map[string]json.RawMessage
	if json.Unmarshal(configuration["containers"], &containers) != nil || len(containers) != 1 {
		return errors.New("test gateway requires one legacy container")
	}
	encoded, err := json.Marshal(map[string]string{"PR_REVIEW_AGENT_IMAGE": image})
	if err != nil {
		return errors.New("baseline image could not be encoded")
	}
	containers[0]["image_vars"] = encoded
	if err := setWranglerValue(wranglerConfiguration(containers[0]), "name", operation.name); err != nil {
		return err
	}
	if err := setWranglerValue(configuration, "containers", containers); err != nil {
		return err
	}
	if err := operation.upload(ctx, "bootstrap-legacy", template, configuration, false); err != nil {
		return err
	}
	return operation.waitTestGateway(ctx, revision)
}

func validateTestBaselineBindings(configuration wranglerConfiguration) error {
	var durable struct {
		Bindings []map[string]json.RawMessage `json:"bindings"`
	}
	if json.Unmarshal(configuration["durable_objects"], &durable) != nil || len(durable.Bindings) == 0 {
		return errors.New("baseline Worker requires local Durable Object bindings")
	}
	fields := map[string]bool{"name": true, "class_name": true}
	for _, binding := range durable.Bindings {
		for field := range binding {
			if !fields[field] {
				return errors.New("baseline Durable Object bindings must reference local classes without external namespace or script fields")
			}
		}
	}
	var migrations []map[string]json.RawMessage
	if json.Unmarshal(configuration["migrations"], &migrations) != nil || len(migrations) == 0 {
		return errors.New("baseline Worker requires fresh local SQLite migrations")
	}
	for _, migration := range migrations {
		for field := range migration {
			if field != "tag" && field != "new_sqlite_classes" {
				return errors.New("baseline migrations must create only local SQLite classes")
			}
		}
	}
	var services []serviceBinding
	if raw, exists := configuration["services"]; exists && (json.Unmarshal(raw, &services) != nil || len(services) > 0) {
		return errors.New("baseline Worker must not reference external services")
	}
	return nil
}

func (operation *deployment) waitTestGateway(ctx context.Context, revision string) error {
	timeout, _ := time.ParseDuration(operation.configuration.CandidateTimeout)
	interval, _ := time.ParseDuration(operation.configuration.ProbeInterval)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		if _, err := operation.probe(ctx, operation.configuration.GatewayOrigin, revision); err == nil {
			return nil
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return errors.New("isolated legacy gateway did not become healthy")
		case <-timer.C:
		}
	}
}

func (operation *deployment) testSnapshot(ctx context.Context) (testDeploymentSnapshot, error) {
	var snapshot testDeploymentSnapshot
	settings, _, err := operation.client.workerSettings(ctx, operation.account, operation.name, false)
	if err != nil {
		return snapshot, err
	}
	snapshot.Selection, err = settings.selection(operation.configuration)
	if err != nil {
		return snapshot, err
	}
	snapshot.Bindings = make(map[string]testBindingMetadata)
	for _, binding := range settings.Bindings {
		valueDigest := ""
		if binding.Text != "" {
			digest := sha256.Sum256([]byte(binding.Text))
			valueDigest = hex.EncodeToString(digest[:])
		}
		snapshot.Bindings[binding.Name] = testBindingMetadata{Type: binding.Type, NamespaceID: binding.NamespaceID, ClassName: binding.ClassName, Service: binding.Service, Entrypoint: binding.Entrypoint, Environment: binding.Environment, ValueSHA256: valueDigest}
	}
	snapshot.Revision, err = operation.probe(ctx, operation.configuration.GatewayOrigin, "")
	if err != nil {
		return snapshot, err
	}
	snapshot.Applications, err = operation.testApplications(ctx)
	if err != nil {
		return snapshot, err
	}
	snapshot.WorkerDeploymentID, snapshot.WorkerVersionID, err = operation.testWorkerVersion(ctx)
	return snapshot, err
}

func (operation *deployment) testWorkerVersion(ctx context.Context) (string, string, error) {
	return operation.testWorkerVersionNamed(ctx, operation.name)
}

func (operation *deployment) testWorkerVersionNamed(ctx context.Context, name string) (string, string, error) {
	response, _, err := operation.client.request(ctx, http.MethodGet, "/accounts/"+operation.account+"/workers/scripts/"+name+"/deployments", nil)
	if err != nil {
		return "", "", err
	}
	var history struct {
		Deployments []struct {
			ID       string `json:"id"`
			Versions []struct {
				ID         string  `json:"version_id"`
				Percentage float64 `json:"percentage"`
			} `json:"versions"`
		} `json:"deployments"`
	}
	if json.Unmarshal(response.Result, &history) != nil || len(history.Deployments) == 0 {
		return "", "", errors.New("test Worker deployment history is unavailable")
	}
	current := history.Deployments[0]
	if current.ID == "" || len(current.Versions) != 1 || current.Versions[0].ID == "" || current.Versions[0].Percentage != 100 {
		return "", "", errors.New("test Worker does not select one complete version")
	}
	return current.ID, current.Versions[0].ID, nil
}

func (operation *deployment) testApplications(ctx context.Context) ([]testContainerApplication, error) {
	selected := make([]testContainerApplication, 0)
	for _, name := range []string{operation.name, operation.configuration.Slots[0].Name, operation.configuration.Slots[1].Name} {
		applications, err := operation.testApplicationsNamed(ctx, name)
		if err != nil {
			return nil, err
		}
		selected = append(selected, applications...)
	}
	return selected, nil
}

func (operation *deployment) testApplicationsNamed(ctx context.Context, name string) ([]testContainerApplication, error) {
	response, _, err := operation.client.request(ctx, http.MethodGet, "/accounts/"+operation.account+"/containers/applications?name="+url.QueryEscape(name), nil)
	if err != nil {
		return nil, err
	}
	var applications []testContainerApplication
	if json.Unmarshal(response.Result, &applications) != nil || len(applications) > 1 {
		return nil, errors.New("exact-name container application list is invalid or ambiguous")
	}
	for _, application := range applications {
		if application.Name != name || application.ID == "" || application.Configuration.Image == "" {
			return nil, errors.New("test container application omitted its exact name, ID, or image")
		}
	}
	return applications, nil
}

func verifyRejectedCandidate(before, after testDeploymentSnapshot) error {
	if before.Selection != after.Selection || before.Revision != after.Revision {
		return errors.New("rejected candidate changed active selection or revision")
	}
	if !reflect.DeepEqual(before.Bindings, after.Bindings) {
		return errors.New("rejected candidate changed gateway bindings")
	}
	matched := false
	for _, old := range before.Applications {
		for _, current := range after.Applications {
			if current.ID == old.ID {
				if current.Configuration.Image != old.Configuration.Image {
					return errors.New("rejected candidate changed the active application image")
				}
				matched = true
			}
		}
	}
	if !matched {
		return errors.New("active baseline application disappeared after candidate rejection")
	}
	return nil
}

type testReadinessSample struct {
	Timestamp time.Time `json:"timestamp"`
	Status    int       `json:"status"`
	Revision  string    `json:"revision"`
	Error     string    `json:"error,omitempty"`
}

func (operation *deployment) sampleTestGateway(ctx context.Context, revisions []string, writer io.Writer) (int, int, error) {
	encoder := json.NewEncoder(writer)
	count, failed := 0, 0
	for {
		sample := operation.readTestGateway(ctx)
		if ctx.Err() != nil {
			return count, failed, context.Canceled
		}
		count++
		if sample.Status != http.StatusOK || !testRevisionAllowed(sample.Revision, revisions) {
			failed++
		}
		if err := encoder.Encode(sample); err != nil {
			return count, failed, errors.New("readiness evidence could not be written")
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return count, failed, context.Canceled
		case <-timer.C:
		}
	}
}

func testRevisionAllowed(actual string, revisions []string) bool {
	if !reportedRevisionPattern.MatchString(actual) {
		return false
	}
	for _, revision := range revisions {
		if strings.HasPrefix(revision, actual) {
			return true
		}
	}
	return false
}

func (operation *deployment) readTestGateway(ctx context.Context) testReadinessSample {
	var sample testReadinessSample
	sample.Timestamp = clock.System().UTC()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, operation.configuration.GatewayOrigin+"/", nil)
	if err != nil {
		sample.Error = "readiness request could not be created"
		return sample
	}
	response, err := operation.healthClient.Do(request)
	if err != nil {
		sample.Error = "readiness transport failed"
		return sample
	}
	defer func() { _ = response.Body.Close() }()
	sample.Status, sample.Revision = response.StatusCode, response.Header.Get("X-Pr-Agent-Revision")
	return sample
}

func (operation *deployment) testRejectedCandidate(ctx context.Context, options DeploymentOptions, baseline string) (int, int, error) {
	observed, err := operation.observeTestDeployment(ctx, options, []string{baseline}, "availability.ndjson", Deploy)
	if err != nil {
		return observed.samples.Count, observed.samples.Failed, err
	}
	if !errors.Is(observed.deploymentError, errCandidateReadiness) {
		if observed.deploymentError == nil {
			return observed.samples.Count, observed.samples.Failed, errors.New("mismatched candidate unexpectedly promoted")
		}
		slog.WarnContext(ctx, "Candidate test failed before expected readiness comparison")
		return observed.samples.Count, observed.samples.Failed, fmt.Errorf("candidate did not fail its expected readiness comparison: %w", observed.deploymentError)
	}
	return observed.samples.Count, observed.samples.Failed, nil
}

func (operation *deployment) testSuccessfulCandidate(ctx context.Context, options DeploymentOptions, revisions []string, filename string) (DeploymentResult, testAvailabilityCounts, error) {
	observed, err := operation.observeTestDeployment(ctx, options, revisions, filename, Deploy)
	if err != nil {
		return observed.deployment, observed.samples, err
	}
	if observed.deploymentError != nil {
		return observed.deployment, observed.samples, observed.deploymentError
	}
	if !observed.deployment.Promoted {
		return observed.deployment, observed.samples, errors.New("candidate did not promote")
	}
	return observed.deployment, observed.samples, nil
}

func (operation *deployment) observeTestDeployment(ctx context.Context, options DeploymentOptions, revisions []string, filename string, deploy func(context.Context, DeploymentOptions) (DeploymentResult, error)) (observed observedTestDeployment, returnErr error) {
	file, err := os.OpenFile(filepath.Join(operation.directory, filename), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		slog.WarnContext(ctx, "Availability evidence creation failed")
		return observed, fmt.Errorf("create availability evidence: %w", err)
	}
	defer func() { _ = file.Close() }()
	type sampleResult struct {
		count  int
		failed int
		err    error
	}
	results := make(chan sampleResult, 1)
	sampling, cancel := context.WithCancel(ctx)
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				slog.ErrorContext(sampling, "Availability sampler panicked", "err", errors.New("availability sampler panicked"))
				results <- sampleResult{count: 0, failed: 0, err: errors.New("availability sampler panicked")}
			}
		}()
		count, failed, err := operation.sampleTestGateway(sampling, revisions, file)
		results <- sampleResult{count: count, failed: failed, err: err}
	}()
	observed.deployment, observed.deploymentError = deploy(ctx, options)
	cancel()
	samples := <-results
	observed.samples = testAvailabilityCounts{Count: samples.count, Failed: samples.failed}
	if samples.count == 0 {
		return observed, errors.New("availability proof did not measure any responses")
	}
	if samples.err != nil && !errors.Is(samples.err, context.Canceled) {
		return observed, samples.err
	}
	if samples.failed > 0 {
		return observed, errors.New("active gateway failed during candidate upload, handoff, or promotion")
	}
	return observed, nil
}

func (operation *deployment) cleanupTestbed(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	applications, err := operation.testApplications(ctx)
	if err != nil {
		return err
	}
	for _, application := range applications {
		if _, _, err := operation.client.request(ctx, http.MethodDelete, "/accounts/"+operation.account+"/containers/applications/"+application.ID, nil); err != nil {
			return err
		}
	}
	for _, name := range []string{operation.configuration.Slots[0].Name, operation.configuration.Slots[1].Name, operation.name} {
		_, exists, err := operation.client.workerSettings(ctx, operation.account, name, true)
		if err != nil {
			return err
		}
		if !exists {
			continue
		}
		if _, _, err := operation.client.request(ctx, http.MethodDelete, "/accounts/"+operation.account+"/workers/scripts/"+name+"?force=true", nil); err != nil {
			return err
		}
		_, exists, err = operation.client.workerSettings(ctx, operation.account, name, true)
		if err != nil {
			return err
		}
		if exists {
			return errors.New("isolated Worker still exists after cleanup")
		}
	}
	remaining, err := operation.testApplications(ctx)
	if err != nil {
		return err
	}
	if len(remaining) > 0 {
		return errors.New("isolated container applications remain after cleanup")
	}
	return nil
}
