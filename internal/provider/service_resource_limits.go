package provider

import (
	"context"
	"fmt"

	"github.com/Khan/genqlient/graphql"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// Like the other railway_service settings, limits apply to every instance.
func updateServiceResourceLimits(ctx context.Context, client graphql.Client, data *ServiceResourceModel) error {
	input := ServiceInstanceLimitsUpdateInput{ServiceId: data.Id.ValueString()}

	if !data.CPULimit.IsNull() && !data.CPULimit.IsUnknown() {
		input.VCPUs = data.CPULimit.ValueFloat64Pointer()
	}

	if !data.MemoryLimitGB.IsNull() && !data.MemoryLimitGB.IsUnknown() {
		input.MemoryGB = data.MemoryLimitGB.ValueFloat64Pointer()
	}

	if input.VCPUs == nil && input.MemoryGB == nil {
		return nil
	}

	instances, err := getServiceInstances(ctx, client, data.Id.ValueString())
	if err != nil {
		return err
	}

	if len(instances.Service.ServiceInstances.Edges) == 0 {
		return fmt.Errorf("service %s has no instances to configure", data.Id.ValueString())
	}

	for _, instance := range instances.Service.ServiceInstances.Edges {
		input.EnvironmentId = instance.Node.EnvironmentId

		if _, err := setServiceResourceLimits(ctx, client, input); err != nil {
			return fmt.Errorf("environment %s: %w", input.EnvironmentId, err)
		}
	}

	return nil
}

func readServiceResourceLimits(ctx context.Context, client graphql.Client, environmentID, serviceID string, data *ServiceResourceModel) error {
	response, err := getServiceResourceLimits(ctx, client, environmentID)
	if err != nil {
		return err
	}

	// Read configured overrides rather than the plan's merged default limits.
	// Otherwise an unrelated update could turn those defaults into overrides.
	services, ok := response.Environment.Config["services"].(map[string]interface{})
	if !ok {
		return fmt.Errorf("environment configuration is missing services")
	}

	service, ok := services[serviceID].(map[string]interface{})
	if !ok {
		return fmt.Errorf("environment configuration is missing service %s", serviceID)
	}

	deploy, _ := service["deploy"].(map[string]interface{})
	limits, _ := deploy["limitOverride"].(map[string]interface{})
	containers, _ := limits["containers"].(map[string]interface{})

	data.CPULimit = types.Float64Null()
	if cpu, ok := containers["cpu"].(float64); ok {
		data.CPULimit = types.Float64Value(cpu)
	}

	data.MemoryLimitGB = types.Float64Null()
	if memory, ok := containers["memoryBytes"].(float64); ok {
		data.MemoryLimitGB = types.Float64Value(memory / 1_000_000_000)
	}

	return nil
}
