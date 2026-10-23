package cloudflareops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

var errCandidateReadiness = errors.New("candidate did not become healthy before promotion")

type readinessHTTPError struct {
	StatusCode int
}

func (err *readinessHTTPError) Error() string {
	return fmt.Sprintf("health returned HTTP %d", err.StatusCode)
}

// DeploymentOptions selects public templates and file-backed deployment credentials.
type DeploymentOptions struct {
	Wrangler         string
	ServiceWrangler  string
	Configuration    string
	Image            string
	Revision         string
	TokenFile        string
	AccountTokenFile string
	CredentialTTL    time.Duration
	Directory        string
	Preflight        bool
}

// DeploymentResult identifies the original route and the independently prepared candidate.
type DeploymentResult struct {
	Gateway                string `json:"gateway"`
	PreviousMode           string `json:"previous_mode"`
	PreviousService        string `json:"previous_service,omitempty"`
	Candidate              string `json:"candidate"`
	Revision               string `json:"revision,omitempty"`
	Promoted               bool   `json:"promoted"`
	Directory              string `json:"directory"`
	PendingHandoffRevision string `json:"pending_handoff_revision,omitempty"`
}

// MarshalJSON encodes public deployment identifiers without settings or credentials.
func (result DeploymentResult) MarshalJSON() ([]byte, error) {
	type wireResult DeploymentResult
	data, err := json.Marshal(wireResult(result))
	if err != nil {
		return nil, fmt.Errorf("encode deployment result: %w", err)
	}
	return data, nil
}

type deployment struct {
	client          *Client
	options         DeploymentOptions
	configuration   DeploymentConfiguration
	gateway         wranglerConfiguration
	service         wranglerConfiguration
	account         string
	name            string
	before          workerSettings
	selection       deploymentSelection
	slot            DeploymentSlot
	directory       string
	healthClient    *http.Client
	activeRevision  string
	candidateExists bool
	handoff         deploymentHandoff
	environment     []string
}

// Deploy preserves the selected application while it validates an independent candidate.
func Deploy(ctx context.Context, options DeploymentOptions) (_ DeploymentResult, returnErr error) {
	operation, err := prepareDeployment(options)
	if err != nil {
		return DeploymentResult{}, err
	}
	permissions := []string{"Workers Scripts Write", "Workers Containers Write"}
	if options.Preflight {
		permissions = []string{"Workers Scripts Read"}
	}
	credentials, err := AcquireCredentials(ctx, CredentialOptions{TokenFile: options.TokenFile, AccountTokenFile: options.AccountTokenFile, Account: operation.account, TTL: options.CredentialTTL, APIURL: APIURL, PermissionNames: permissions})
	if err != nil {
		return DeploymentResult{}, err
	}
	defer func() { returnErr = errors.Join(returnErr, credentials.Close(ctx)) }()
	operation.client = credentials.Client
	if err := operation.preflight(ctx); err != nil {
		return DeploymentResult{}, err
	}
	result := DeploymentResult{Gateway: operation.name, PreviousMode: operation.selection.Mode, PreviousService: operation.selection.Service, Candidate: operation.slot.Name, Revision: options.Revision, Promoted: false, Directory: "", PendingHandoffRevision: operation.handoff.Revision}
	if options.Preflight {
		return result, nil
	}
	operation.directory, err = PrivateDirectory(options.Directory)
	if err != nil {
		return result, err
	}
	result.Directory = operation.directory
	if err := WriteJSON(filepath.Join(operation.directory, "selection.json"), result); err != nil {
		return result, err
	}
	if err := operation.execute(ctx); err != nil {
		return result, err
	}
	result.Promoted = true
	if err := WriteJSON(filepath.Join(operation.directory, "result.json"), result); err != nil {
		return result, err
	}
	return result, nil
}

func prepareDeployment(options DeploymentOptions) (*deployment, error) {
	if !options.Preflight && (!imageDigestPattern.MatchString(options.Image) || !revisionPattern.MatchString(options.Revision)) {
		return nil, errors.New("deployment requires an immutable GHCR image digest and a full lowercase Git revision")
	}
	configuration, err := readDeploymentConfiguration(options.Configuration)
	if err != nil {
		return nil, err
	}
	gateway, err := readWrangler(options.Wrangler)
	if err != nil {
		return nil, err
	}
	service, err := readWrangler(options.ServiceWrangler)
	if err != nil {
		return nil, err
	}
	account, name := gateway.stringField("account_id"), gateway.stringField("name")
	if len(account) != 32 || !workerNamePattern.MatchString(name) {
		return nil, errors.New("gateway template requires a valid account_id and Worker name")
	}
	for _, slot := range configuration.Slots {
		if slot.Name == name {
			return nil, errors.New("candidate Worker name must differ from gateway name")
		}
	}
	return &deployment{client: nil, options: options, configuration: configuration, gateway: gateway, service: service, account: account, name: name, before: workerSettings{Bindings: nil}, selection: deploymentSelection{Mode: "", Service: ""}, slot: DeploymentSlot{Name: "", Origin: ""}, directory: "", activeRevision: "", candidateExists: false, handoff: deploymentHandoff{Enabled: false, Revision: "", Service: ""}, environment: os.Environ(), healthClient: &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

func (operation *deployment) preflight(ctx context.Context) error {
	settings, _, err := operation.client.workerSettings(ctx, operation.account, operation.name, false)
	if err != nil {
		return err
	}
	selection, err := settings.selection(operation.configuration)
	if err != nil {
		return err
	}
	if err := validateGatewayBindings(operation.gateway, settings); err != nil {
		return err
	}
	operation.before, operation.selection = settings, selection
	operation.handoff, err = settings.handoff(operation.configuration)
	if err != nil {
		return err
	}
	if operation.handoff.Enabled && selection.Mode == "service" && operation.handoff.Service == selection.Service {
		return errors.New("handoff candidate must differ from the active service")
	}
	operation.slot = inactiveSlot(operation.configuration, selection)
	if operation.handoff.Enabled {
		for _, slot := range operation.configuration.Slots {
			if slot.Name == operation.handoff.Service {
				operation.slot = slot
			}
		}
	}
	operation.activeRevision, err = operation.probe(ctx, operation.configuration.GatewayOrigin, "")
	if err != nil {
		slog.WarnContext(ctx, "Active gateway readiness failed")
		return fmt.Errorf("active gateway is not healthy: %w", err)
	}
	for _, slot := range operation.configuration.Slots {
		candidate, exists, err := operation.client.workerSettings(ctx, operation.account, slot.Name, true)
		if err != nil {
			return err
		}
		if exists {
			if err := candidateOwnedByGateway(candidate, operation.name); err != nil {
				slog.WarnContext(ctx, "Candidate ownership check failed", "slot", slot.Name)
				return fmt.Errorf("slot %s: %w", slot.Name, err)
			}
		}
		if slot.Name == operation.slot.Name {
			operation.candidateExists = exists
		}
	}
	return nil
}

func (operation *deployment) execute(ctx context.Context) error {
	if operation.handoff.Enabled {
		return operation.resumeHandoff(ctx)
	}
	if err := configureCandidate(operation.service, operation.slot, operation.account, operation.name, operation.options.Image); err != nil {
		return err
	}
	if err := operation.requireOriginalSelection(ctx); err != nil {
		return err
	}
	if operation.candidateExists {
		if err := operation.waitDrained(ctx, operation.slot); err != nil {
			return err
		}
	}
	if err := operation.requireOriginalSelection(ctx); err != nil {
		return err
	}
	if err := configureGateway(operation.gateway, operation.selection, operation.before); err != nil {
		return err
	}
	if err := operation.upload(ctx, "prepare-gateway", operation.options.Wrangler, operation.gateway, true); err != nil {
		return err
	}
	if err := operation.requireOriginalSelection(ctx); err != nil {
		return err
	}
	if _, err := operation.probe(ctx, operation.configuration.GatewayOrigin, operation.activeRevision); err != nil {
		slog.WarnContext(ctx, "Gateway readiness failed after preparation")
		return fmt.Errorf("gateway preparation failed health: %w", err)
	}
	if err := operation.upload(ctx, "deploy-candidate", operation.options.ServiceWrangler, operation.service, false); err != nil {
		return err
	}
	if err := operation.registerDeploymentToken(ctx, operation.slot); err != nil {
		return err
	}
	if _, err := operation.drainRequest(ctx, operation.slot.Origin, "restart"); err != nil {
		return err
	}
	if err := operation.waitCandidate(ctx); err != nil {
		return err
	}
	if err := operation.requireOriginalSelection(ctx); err != nil {
		return err
	}
	return operation.startHandoff(ctx)
}

func (operation *deployment) requireOriginalSelection(ctx context.Context) error {
	settings, _, err := operation.client.workerSettings(ctx, operation.account, operation.name, false)
	if err != nil {
		return err
	}
	selection, err := settings.selection(operation.configuration)
	if err != nil {
		return err
	}
	if selection != operation.selection {
		return errors.New("active service changed during candidate preparation; refusing promotion")
	}
	handoff, err := settings.handoff(operation.configuration)
	if err != nil {
		return err
	}
	if handoff != operation.handoff {
		return errors.New("deployment handoff changed during preparation; refusing to replace its candidate")
	}
	return preserveBindings(operation.before, settings)
}

func (operation *deployment) upload(ctx context.Context, stage, template string, configuration wranglerConfiguration, gateway bool) (returnErr error) {
	slog.InfoContext(ctx, "Deployment stage started", "stage", stage)
	path, err := writeWrangler(template, configuration)
	if err != nil {
		slog.WarnContext(ctx, "Wrangler configuration path failed")
		return fmt.Errorf("resolve Wrangler configuration path: %w", err)
	}
	defer func() { returnErr = errors.Join(returnErr, os.Remove(path)) }()
	absolutePath, err := filepath.Abs(path)
	if err != nil {
		slog.WarnContext(ctx, "Wrangler configuration path resolution failed")
		return fmt.Errorf("resolve Wrangler configuration path: %w", err)
	}
	directory, err := filepath.Abs(filepath.Dir(template))
	if err != nil {
		slog.WarnContext(ctx, "Wrangler template directory failed")
		return fmt.Errorf("resolve Wrangler template directory: %w", err)
	}
	log, err := os.OpenFile(filepath.Join(operation.directory, stage+".log"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		slog.WarnContext(ctx, "Deployment log creation failed")
		return fmt.Errorf("create deployment log: %w", err)
	}
	defer func() { returnErr = errors.Join(returnErr, log.Close()) }()
	args := []string{filepath.Join(directory, "node_modules", "wrangler", "bin", "wrangler.js"), "deploy", "--config", absolutePath}
	if gateway {
		args = append(args, "--containers-rollout=none")
	}
	command := exec.CommandContext(ctx, "node", args...)
	command.Dir = directory
	command.Env = deploymentEnvironment(operation.environment, operation.client.token, operation.account, operation.directory)
	command.Stdout, command.Stderr = log, log
	if err := command.Run(); err != nil {
		slog.WarnContext(ctx, "Deployment stage failed", "stage", stage, "log", log.Name())
		return fmt.Errorf("%s failed; private deployment log %s: %w", stage, log.Name(), err)
	}
	return nil
}

func deploymentEnvironment(inherited []string, token, account, directory string) []string {
	environment := make([]string, 0)
	for _, entry := range inherited {
		if strings.HasPrefix(entry, "CLOUDFLARE_") || strings.HasPrefix(entry, "WRANGLER_") {
			continue
		}
		environment = append(environment, entry)
	}
	return append(environment, "CLOUDFLARE_API_TOKEN="+token, "CLOUDFLARE_ACCOUNT_ID="+account, "WRANGLER_SEND_METRICS=false", "WRANGLER_LOG=warn", "WRANGLER_LOG_PATH="+filepath.Join(directory, "wrangler.log"))
}

func (operation *deployment) probe(ctx context.Context, origin, revision string) (string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(origin, "/")+"/", nil)
	if err != nil {
		return "", errors.New("health request could not be created")
	}
	response, err := operation.healthClient.Do(request)
	if err != nil {
		slog.WarnContext(ctx, "Deployment health transport failed", "origin", origin)
		return "", fmt.Errorf("health request failed: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64*1024))
	actual := response.Header.Get("X-Pr-Agent-Revision")
	if response.StatusCode != http.StatusOK {
		return actual, &readinessHTTPError{StatusCode: response.StatusCode}
	}
	if revision != "" && actual != revision {
		return actual, fmt.Errorf("health revision %q does not match expected revision %s", actual, revision)
	}
	return actual, nil
}

func (operation *deployment) waitCandidate(ctx context.Context) error {
	timeout, interval := operation.configuration.candidateTimeout, operation.configuration.probeInterval
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var lastError error
	for {
		if _, err := operation.probe(ctx, operation.configuration.GatewayOrigin, operation.activeRevision); err != nil {
			slog.WarnContext(ctx, "Active gateway readiness failed during warmup")
			return fmt.Errorf("active gateway failed during candidate warmup: %w", err)
		}
		_, lastError = operation.probe(ctx, operation.slot.Origin, operation.options.Revision)
		if lastError == nil {
			return nil
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			slog.WarnContext(ctx, "Candidate warmup exceeded its elapsed limit", "candidate", operation.slot.Name)
			return fmt.Errorf("%w: %w", errCandidateReadiness, errors.Join(lastError, ctx.Err()))
		case <-timer.C:
		}
	}
}
