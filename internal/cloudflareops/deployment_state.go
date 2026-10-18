package cloudflareops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"reflect"
)

type workerBindingType string

const (
	serviceBindingType       workerBindingType = "service"
	plainTextBindingType     workerBindingType = "plain_text"
	secretTextBindingType    workerBindingType = "secret_text"
	durableObjectBindingType workerBindingType = "durable_object_namespace"
)

type workerBinding struct {
	Name        string            `json:"name"`
	Type        workerBindingType `json:"type"`
	Text        string            `json:"text"`
	Service     string            `json:"service"`
	Entrypoint  string            `json:"entrypoint"`
	Environment string            `json:"environment"`
	NamespaceID string            `json:"namespace_id"`
	ClassName   string            `json:"class_name"`
}

type workerSettings struct {
	Bindings []workerBinding `json:"bindings"`
}

type deploymentSelection struct {
	Mode    string `json:"mode"`
	Service string `json:"service,omitempty"`
}

func (client *Client) workerSettings(ctx context.Context, account, script string, allowMissing bool) (workerSettings, bool, error) {
	response, status, err := client.request(ctx, http.MethodGet, "/accounts/"+account+"/workers/scripts/"+script+"/settings", nil)
	if allowMissing && status == http.StatusNotFound && len(response.Errors) == 1 && response.Errors[0].Code == 10007 {
		return workerSettings{Bindings: nil}, false, nil
	}
	if err != nil {
		slog.WarnContext(ctx, "Worker settings read failed", "script", script)
		return workerSettings{}, false, fmt.Errorf("read Worker %s settings: %w", script, err)
	}
	var settings workerSettings
	if json.Unmarshal(response.Result, &settings) != nil || settings.Bindings == nil {
		return settings, false, errors.New("worker settings omitted readable bindings")
	}
	seen := make(map[string]bool)
	for _, binding := range settings.Bindings {
		if binding.Name == "" || seen[binding.Name] {
			return settings, false, errors.New("worker settings contain missing or duplicate binding names")
		}
		seen[binding.Name] = true
	}
	return settings, true, nil
}

func (settings workerSettings) binding(name string) (workerBinding, bool) {
	for _, binding := range settings.Bindings {
		if binding.Name == name {
			return binding, true
		}
	}
	var missing workerBinding
	return missing, false
}

func (settings workerSettings) selection(configuration DeploymentConfiguration) (deploymentSelection, error) {
	mode, exists := settings.binding("REVIEW_SERVICE_MODE")
	if exists && mode.Type != "plain_text" {
		return deploymentSelection{}, errors.New("deployed REVIEW_SERVICE_MODE must be plain text")
	}
	service, serviceExists := settings.binding("REVIEW_SERVICE")
	if !exists || mode.Text == "legacy" {
		legacy, legacyExists := settings.binding("PR_AGENT")
		if serviceExists || !legacyExists || legacy.Type != "durable_object_namespace" || legacy.ClassName != "PrAgentContainer" || legacy.NamespaceID == "" {
			return deploymentSelection{}, errors.New("legacy bootstrap requires the deployed PR_AGENT container binding without REVIEW_SERVICE")
		}
		return deploymentSelection{Mode: "legacy", Service: ""}, nil
	}
	if mode.Text != "service" || !serviceExists || service.Type != "service" || service.Entrypoint != "ReviewService" || service.Environment != "" {
		return deploymentSelection{}, errors.New("deployed review service selection is invalid")
	}
	for _, slot := range configuration.Slots {
		if service.Service == slot.Name {
			return deploymentSelection{Mode: "service", Service: slot.Name}, nil
		}
	}
	return deploymentSelection{}, errors.New("deployed REVIEW_SERVICE selects an unknown Worker; update the explicit deployment slots")
}

func inactiveSlot(configuration DeploymentConfiguration, selection deploymentSelection) DeploymentSlot {
	for _, slot := range configuration.Slots {
		if slot.Name != selection.Service {
			return slot
		}
	}
	return DeploymentSlot{Name: "", Origin: ""}
}

func candidateOwnedByGateway(settings workerSettings, gateway string) error {
	binding, exists := settings.binding("RUNTIME_CONFIGURATION")
	if !exists || binding.Type != "service" || binding.Service != gateway || binding.Entrypoint != "RuntimeConfiguration" || binding.Environment != "" {
		return errors.New("existing candidate Worker does not bind the expected gateway RuntimeConfiguration; refusing to overwrite it")
	}
	return nil
}

func preserveBindings(before, after workerSettings) error {
	for _, binding := range before.Bindings {
		if binding.Name == "REVIEW_SERVICE_MODE" || binding.Name == "REVIEW_SERVICE" {
			continue
		}
		current, exists := after.binding(binding.Name)
		if !exists || !reflect.DeepEqual(binding, current) {
			return fmt.Errorf("gateway binding %s changed during preparation", binding.Name)
		}
	}
	return nil
}

func validateGatewayBindings(configuration wranglerConfiguration, settings workerSettings) error {
	var durable struct {
		Bindings []struct {
			Name      string `json:"name"`
			ClassName string `json:"class_name"`
		} `json:"bindings"`
	}
	if json.Unmarshal(configuration["durable_objects"], &durable) != nil || len(durable.Bindings) == 0 {
		return errors.New("gateway template requires existing Durable Object bindings")
	}
	for _, configured := range durable.Bindings {
		deployed, exists := settings.binding(configured.Name)
		if !exists || deployed.Type != "durable_object_namespace" || deployed.NamespaceID == "" || deployed.ClassName != configured.ClassName {
			return fmt.Errorf("gateway template Durable Object %s does not match its deployed class and namespace", configured.Name)
		}
	}
	for _, deployed := range settings.Bindings {
		switch deployed.Type {
		case serviceBindingType, plainTextBindingType, secretTextBindingType:
		case durableObjectBindingType:
			found := false
			for _, configured := range durable.Bindings {
				if configured.Name == deployed.Name {
					found = true
				}
			}
			if !found {
				return fmt.Errorf("gateway template omits existing Durable Object binding %s", deployed.Name)
			}
		default:
			return fmt.Errorf("gateway binding %s has unsupported type %s; preserve it explicitly before deployment", deployed.Name, deployed.Type)
		}
	}
	return nil
}
