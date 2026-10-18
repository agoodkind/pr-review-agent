package cloudflareops

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

var (
	workerNamePattern       = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
	imageDigestPattern      = regexp.MustCompile(`^ghcr\.io/[a-z0-9._/-]+@sha256:[a-f0-9]{64}$`)
	revisionPattern         = regexp.MustCompile(`^[a-f0-9]{40}$`)
	reportedRevisionPattern = regexp.MustCompile(`^[a-f0-9]{7,40}$`)
)

// DeploymentSlot identifies an independent application and its public readiness endpoint.
type DeploymentSlot struct {
	Name   string `json:"name"`
	Origin string `json:"origin"`
}

// DeploymentConfiguration controls candidate selection and elapsed readiness limits.
type DeploymentConfiguration struct {
	GatewayOrigin    string           `json:"gateway_origin"`
	Slots            []DeploymentSlot `json:"slots"`
	CandidateTimeout string           `json:"candidate_timeout"`
	ProbeInterval    string           `json:"probe_interval"`
}

type wranglerConfiguration map[string]json.RawMessage

func readDeploymentConfiguration(path string) (DeploymentConfiguration, error) {
	var configuration DeploymentConfiguration
	data, err := os.ReadFile(path)
	if err != nil {
		slog.Warn("Deployment configuration read failed", "path", path)
		return configuration, fmt.Errorf("read deployment configuration: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&configuration); err != nil {
		return configuration, errors.New("deployment configuration must contain valid JSON")
	}
	if len(configuration.Slots) != 2 {
		return configuration, errors.New("deployment configuration requires exactly two independent slots")
	}
	if err := validateOrigin(configuration.GatewayOrigin); err != nil {
		return configuration, err
	}
	for _, slot := range configuration.Slots {
		if !workerNamePattern.MatchString(slot.Name) {
			return configuration, errors.New("deployment slot requires a valid Worker name")
		}
		if err := validateOrigin(slot.Origin); err != nil {
			return configuration, err
		}
		if slot.Origin == configuration.GatewayOrigin {
			return configuration, errors.New("candidate origin must differ from gateway origin")
		}
	}
	if configuration.Slots[0].Name == configuration.Slots[1].Name || configuration.Slots[0].Origin == configuration.Slots[1].Origin {
		return configuration, errors.New("deployment slots must have distinct names and origins")
	}
	timeout, err := time.ParseDuration(configuration.CandidateTimeout)
	if err != nil || timeout <= 0 {
		return configuration, errors.New("candidate_timeout must be a positive elapsed duration")
	}
	interval, err := time.ParseDuration(configuration.ProbeInterval)
	if err != nil || interval <= 0 || interval >= timeout {
		return configuration, errors.New("probe_interval must be positive and shorter than candidate_timeout")
	}
	return configuration, nil
}

func validateOrigin(origin string) error {
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return errors.New("deployment origins must contain an HTTPS origin without credentials, path, query, or fragment")
	}
	return nil
}

func readWrangler(path string) (wranglerConfiguration, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		slog.Warn("Wrangler template read failed", "path", path)
		return nil, fmt.Errorf("read Wrangler template: %w", err)
	}
	var configuration wranglerConfiguration
	if json.Unmarshal(data, &configuration) != nil || configuration == nil {
		return nil, errors.New("wrangler template must contain a JSON object")
	}
	return configuration, nil
}

func (configuration wranglerConfiguration) stringField(name string) string {
	var value string
	_ = json.Unmarshal(configuration[name], &value)
	return value
}

type wranglerValue interface {
	string | bool | map[string]json.RawMessage | []serviceBinding | []map[string]json.RawMessage
}

func setWranglerValue[Value wranglerValue](configuration wranglerConfiguration, name string, value Value) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		slog.Warn("Wrangler field encoding failed", "field", name)
		return fmt.Errorf("encode Wrangler field %s: %w", name, err)
	}
	configuration[name] = encoded
	return nil
}

func writeWrangler(path string, configuration wranglerConfiguration) (string, error) {
	slog.Info("Private Wrangler configuration creation started", "template", path)
	file, err := os.CreateTemp(filepath.Dir(path), "wrangler.deploy.*.jsonc")
	if err != nil {
		slog.Warn("Private Wrangler configuration creation failed")
		return "", fmt.Errorf("create private Wrangler configuration: %w", err)
	}
	name := file.Name()
	if err := json.NewEncoder(file).Encode(configuration); err != nil {
		_ = file.Close()
		_ = os.Remove(name)
		return "", errors.New("write private Wrangler configuration failed")
	}
	if err := file.Close(); err != nil {
		slog.Warn("Private Wrangler configuration close failed")
		return "", fmt.Errorf("close Wrangler configuration: %w", err)
	}
	return name, nil
}

func configureGateway(configuration wranglerConfiguration, selection deploymentSelection, settings workerSettings) error {
	variables := make(map[string]json.RawMessage)
	if raw, exists := configuration["vars"]; exists && json.Unmarshal(raw, &variables) != nil {
		return errors.New("gateway vars must contain an object")
	}
	mode, err := json.Marshal(selection.Mode)
	if err != nil {
		slog.Warn("Service mode encoding failed")
		return fmt.Errorf("encode service mode: %w", err)
	}
	variables["REVIEW_SERVICE_MODE"] = mode
	if err := setWranglerValue(configuration, "vars", variables); err != nil {
		return err
	}
	if err := setWranglerValue(configuration, "keep_vars", true); err != nil {
		return err
	}
	services := make([]serviceBinding, 0)
	for _, binding := range settings.Bindings {
		if binding.Type == "service" && binding.Name != "REVIEW_SERVICE" && binding.Name != "CANDIDATE_REVIEW_SERVICE" {
			services = append(services, serviceBinding{Name: binding.Name, Service: binding.Service, Entrypoint: binding.Entrypoint, Environment: binding.Environment})
		}
	}
	if selection.Mode == "service" {
		services = append(services, serviceBinding{Name: "REVIEW_SERVICE", Service: selection.Service, Entrypoint: "ReviewService", Environment: ""})
	}
	return setWranglerValue(configuration, "services", services)
}

type serviceBinding struct {
	Name        string `json:"binding"`
	Service     string `json:"service"`
	Entrypoint  string `json:"entrypoint,omitempty"`
	Environment string `json:"environment,omitempty"`
}

func configureCandidate(configuration wranglerConfiguration, slot DeploymentSlot, account, gateway, image string) error {
	if err := setWranglerValue(configuration, "name", slot.Name); err != nil {
		return err
	}
	if err := setWranglerValue(configuration, "account_id", account); err != nil {
		return err
	}
	if err := setWranglerValue(configuration, "services", []serviceBinding{{Name: "RUNTIME_CONFIGURATION", Service: gateway, Entrypoint: "RuntimeConfiguration", Environment: ""}}); err != nil {
		return err
	}
	var containers []map[string]json.RawMessage
	if json.Unmarshal(configuration["containers"], &containers) != nil || len(containers) != 1 {
		return errors.New("candidate template requires exactly one container")
	}
	var class string
	if json.Unmarshal(containers[0]["class_name"], &class) != nil || class != "ReviewServiceContainer" {
		return errors.New("candidate container must use ReviewServiceContainer")
	}
	if err := setWranglerValue(wranglerConfiguration(containers[0]), "name", slot.Name); err != nil {
		return err
	}
	encoded, err := json.Marshal(map[string]string{"PR_REVIEW_AGENT_IMAGE": image})
	if err != nil {
		slog.Warn("Immutable image encoding failed")
		return fmt.Errorf("encode immutable image: %w", err)
	}
	containers[0]["image_vars"] = encoded
	return setWranglerValue(configuration, "containers", containers)
}
