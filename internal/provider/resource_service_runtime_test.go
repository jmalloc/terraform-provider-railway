package provider

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

const (
	runtimeTestProject    = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	runtimeTestService    = "cccccccc-cccc-cccc-cccc-cccccccccccc"
	runtimeTestProduction = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	runtimeTestStaging    = "dddddddd-dddd-dddd-dddd-dddddddddddd"
)

type serviceRuntimeTestAPI struct {
	original                   http.RoundTripper
	requireLimitsBeforeConnect bool
	mu                         sync.Mutex
	name                       string
	image                      string
	settings                   map[string]interface{}
	limits                     map[string]map[string]interface{}
	limitUpdates               []string
	connections                int
}

func (api *serviceRuntimeTestAPI) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Host != "backboard.railway.app" {
		return api.original.RoundTrip(req)
	}

	api.mu.Lock()
	defer api.mu.Unlock()

	var call struct {
		Operation string `json:"operationName"`
		Variables struct {
			Input         map[string]interface{}
			EnvironmentID string
		}
	}
	if err := json.NewDecoder(req.Body).Decode(&call); err != nil {
		return nil, err
	}

	service := map[string]interface{}{"id": runtimeTestService, "name": api.name, "projectId": runtimeTestProject}
	var data interface{}

	switch call.Operation {
	case "createService", "updateService":
		api.name = call.Variables.Input["name"].(string)
		service["name"] = api.name
		field := "serviceCreate"
		if call.Operation == "updateService" {
			field = "serviceUpdate"
		}
		data = map[string]interface{}{field: service}
	case "getService":
		data = map[string]interface{}{"service": service}
	case "getProject":
		data = map[string]interface{}{"project": map[string]interface{}{
			"id": runtimeTestProject, "name": "runtime-test", "environments": map[string]interface{}{"edges": []interface{}{
				map[string]interface{}{"node": map[string]interface{}{"id": runtimeTestProduction, "name": "production", "createdAt": "2024-01-01T00:00:00Z"}},
				map[string]interface{}{"node": map[string]interface{}{"id": runtimeTestStaging, "name": "staging", "createdAt": "2024-01-02T00:00:00Z"}},
			}},
		}}
	case "getServiceInstances":
		data = map[string]interface{}{"service": map[string]interface{}{"serviceInstances": map[string]interface{}{"edges": []interface{}{
			map[string]interface{}{"node": map[string]interface{}{"id": "production-instance", "environmentId": runtimeTestProduction}},
			map[string]interface{}{"node": map[string]interface{}{"id": "staging-instance", "environmentId": runtimeTestStaging}},
		}}}}
	case "updateServiceInstance":
		for key, value := range call.Variables.Input {
			api.settings[key] = value
		}
		data = map[string]interface{}{"serviceInstanceUpdate": true}
	case "setServiceResourceLimits":
		environmentID := call.Variables.Input["environmentId"].(string)
		if api.limits[environmentID] == nil {
			api.limits[environmentID] = map[string]interface{}{}
		}
		if value, ok := call.Variables.Input["vCPUs"]; ok {
			api.limits[environmentID]["cpu"] = value
		}
		if value, ok := call.Variables.Input["memoryGB"]; ok {
			api.limits[environmentID]["memoryBytes"] = value.(float64) * 1_000_000_000
		}
		api.limitUpdates = append(api.limitUpdates, environmentID)
		data = map[string]interface{}{"serviceInstanceLimitsUpdate": true}
	case "getServiceResourceLimits":
		data = map[string]interface{}{"environment": map[string]interface{}{"config": map[string]interface{}{"services": map[string]interface{}{
			runtimeTestService: map[string]interface{}{"deploy": map[string]interface{}{"limitOverride": map[string]interface{}{"containers": api.limits[call.Variables.EnvironmentID]}}},
		}}}}
	case "getServiceInstance":
		instance := make(map[string]interface{})
		for key, value := range api.settings {
			instance[key] = value
		}
		instance["source"] = map[string]interface{}{"image": api.image}
		instance["latestDeployment"] = map[string]interface{}{"meta": nil}
		data = map[string]interface{}{"serviceInstance": instance}
	case "getVolumeInstances":
		data = map[string]interface{}{"project": map[string]interface{}{"volumes": map[string]interface{}{"edges": []interface{}{}}}}
	case "connectService":
		if api.requireLimitsBeforeConnect && len(api.limitUpdates) != 2 {
			return nil, fmt.Errorf("image connected before configuring both environments resource limits")
		}
		api.image = call.Variables.Input["image"].(string)
		api.connections++
		data = map[string]interface{}{"serviceConnect": service}
	case "redeployServiceInstance":
		data = map[string]interface{}{"serviceInstanceRedeploy": true}
	case "deleteService":
		data = map[string]interface{}{"serviceDelete": true}
	default:
		return nil, fmt.Errorf("unexpected GraphQL operation %q", call.Operation)
	}

	body, err := json.Marshal(map[string]interface{}{"data": data})
	if err != nil {
		return nil, err
	}
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(string(body)))}, nil
}

func mockServiceRuntimeAPI(t *testing.T) *serviceRuntimeTestAPI {
	t.Helper()
	api := &serviceRuntimeTestAPI{settings: map[string]interface{}{"watchPatterns": []string{}}, limits: map[string]map[string]interface{}{}}
	t.Setenv("RAILWAY_TOKEN", "dummy-provider-credential")
	original := http.DefaultTransport
	api.original = original
	http.DefaultTransport = api
	t.Cleanup(func() { http.DefaultTransport = original })
	return api
}

func serviceRuntimeTestConfig(settings string) string {
	return fmt.Sprintf(`
resource "railway_service" "test" {
  name         = "runtime-test"
  project_id   = %q
  source_image = "ghcr.io/example/app:v1.0.0"
  %s
  lifecycle {
    ignore_changes = [source_image]
  }
}
`, runtimeTestProject, settings)
}

func TestServiceRuntimeSettings(t *testing.T) {
	api := mockServiceRuntimeAPI(t)
	settings := `cpu_limit = 0.5
  memory_limit_gb = 0.5
  draining_seconds = 10
  overlap_seconds = 0`
	updated := strings.ReplaceAll(settings, "draining_seconds = 10", "draining_seconds = 15")
	check := func(drain, image string) resource.TestCheckFunc {
		return resource.ComposeAggregateTestCheckFunc(
			resource.TestCheckResourceAttr("railway_service.test", "cpu_limit", "0.5"),
			resource.TestCheckResourceAttr("railway_service.test", "memory_limit_gb", "0.5"),
			resource.TestCheckResourceAttr("railway_service.test", "draining_seconds", drain),
			resource.TestCheckResourceAttr("railway_service.test", "overlap_seconds", "0"),
			resource.TestCheckResourceAttr("railway_service.test", "source_image", image),
		)
	}
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{Config: serviceRuntimeTestConfig("")},
			{Config: serviceRuntimeTestConfig(settings), Check: check("10", "ghcr.io/example/app:v1.0.0")},
			{
				PreConfig: func() {
					api.mu.Lock()
					defer api.mu.Unlock()
					api.limits[runtimeTestProduction]["memoryBytes"] = float64(750_000_000)
				},
				Config: serviceRuntimeTestConfig(settings), Check: check("10", "ghcr.io/example/app:v1.0.0"),
			},
			{
				PreConfig: func() {
					api.mu.Lock()
					defer api.mu.Unlock()
					api.image = "ghcr.io/example/app:v1.0.1"
				},
				Config: serviceRuntimeTestConfig(updated), Check: check("15", "ghcr.io/example/app:v1.0.1"),
			},
			// Omitting optional/computed settings retains the existing values.
			{Config: serviceRuntimeTestConfig(""), Check: check("15", "ghcr.io/example/app:v1.0.1")},
		},
	})

	if api.connections != 1 {
		t.Fatalf("image was reconnected %d times; want only the initial connection", api.connections)
	}
	if got := strings.Join(api.limitUpdates, ","); got != strings.Join([]string{runtimeTestProduction, runtimeTestStaging, runtimeTestProduction, runtimeTestStaging}, ",") {
		t.Fatalf("unexpected resource limit updates: %s", got)
	}
	for _, environmentID := range []string{runtimeTestProduction, runtimeTestStaging} {
		if api.limits[environmentID]["cpu"] != float64(0.5) || api.limits[environmentID]["memoryBytes"] != float64(500_000_000) {
			t.Fatalf("incorrect per-replica limits in %s: %v", environmentID, api.limits[environmentID])
		}
	}
}

func TestServiceRuntimeSettingsValidation(t *testing.T) {
	mockServiceRuntimeAPI(t)
	for _, setting := range []string{"cpu_limit = 0", "memory_limit_gb = -1", "draining_seconds = -1", "overlap_seconds = 2147483648"} {
		t.Run(setting, func(t *testing.T) {
			resource.UnitTest(t, resource.TestCase{
				ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
				Steps:                    []resource.TestStep{{Config: serviceRuntimeTestConfig(setting), ExpectError: regexp.MustCompile("Invalid Attribute Value")}},
			})
		})
	}
}

func TestServiceRuntimeSettingsCreate(t *testing.T) {
	cases := []struct {
		name        string
		settings    string
		cpu, memory float64
	}{
		{"both", "cpu_limit = 0.5\nmemory_limit_gb = 0.5", 0.5, 500_000_000},
		{"cpu only", "cpu_limit = 0.5", 0.5, 250_000_000},
		{"memory only", "memory_limit_gb = 0.5", 0.25, 500_000_000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api := mockServiceRuntimeAPI(t)
			api.requireLimitsBeforeConnect = true
			for _, environmentID := range []string{runtimeTestProduction, runtimeTestStaging} {
				api.limits[environmentID] = map[string]interface{}{"cpu": 0.25, "memoryBytes": float64(250_000_000)}
			}
			resource.UnitTest(t, resource.TestCase{
				ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
				Steps:                    []resource.TestStep{{Config: serviceRuntimeTestConfig(tc.settings + "\ndraining_seconds = 0\noverlap_seconds = 0")}},
			})
			for _, environmentID := range []string{runtimeTestProduction, runtimeTestStaging} {
				if api.limits[environmentID]["cpu"] != tc.cpu || api.limits[environmentID]["memoryBytes"] != tc.memory {
					t.Fatalf("incorrect limits in %s: %v", environmentID, api.limits[environmentID])
				}
			}
		})
	}
}
