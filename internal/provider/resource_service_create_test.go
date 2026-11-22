package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sync"
	"testing"

	"github.com/Khan/genqlient/graphql"
	frameworkprovider "github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

// Exercise the real Terraform retry path against a local GraphQL server. Failed
// creates must remain in state so Terraform can destroy them before retrying.
func TestServiceCreatePartialFailure(t *testing.T) {
	for _, operation := range []string{
		"updateServiceInstance",
		"createVolume",
		"updateVolume",
		"connectService",
		"getServiceInstance",
		"getVolumeInstances",
	} {
		t.Run(operation, func(t *testing.T) {
			fixture := newServiceCreateFixture(t, operation)
			config := fmt.Sprintf(`resource "railway_service" "test" {
  name = "example"
  project_id = %q
  source_image = "ghcr.io/example/app:v1"
  volume = {
    name = "example-data"
    mount_path = "/data"
  }
}`, serviceCreateTestProject)
			resource.UnitTest(t, resource.TestCase{
				ProtoV6ProviderFactories: map[string]func() (tfprotov6.ProviderServer, error){
					"railway": providerserver.NewProtocol6WithError(&serviceCreateTestProvider{client: fixture.client}),
				},
				Steps: []resource.TestStep{
					{Config: config, ExpectError: regexp.MustCompile(`injected\s+` + operation + `\s+failure`)},
					{
						Config: config,
						Check: resource.ComposeAggregateTestCheckFunc(
							resource.TestCheckResourceAttr("railway_service.test", "id", serviceCreateTestService),
							resource.TestCheckResourceAttr("railway_service.test", "volume.id", serviceCreateTestVolume),
							func(state *terraform.State) error {
								fixture.mu.Lock()
								defer fixture.mu.Unlock()
								if fixture.creates != 2 || fixture.deletes != 1 {
									return fmt.Errorf("partial service was not replaced: creates=%d deletes=%d", fixture.creates, fixture.deletes)
								}
								return nil
							},
						),
					},
				},
				CheckDestroy: func(state *terraform.State) error {
					fixture.mu.Lock()
					defer fixture.mu.Unlock()
					if fixture.serviceExists || fixture.volumeExists {
						return fmt.Errorf("service or volume remains after destroy")
					}
					return nil
				},
			})
		})
	}
}

const (
	serviceCreateTestProject     = "10000000-0000-0000-0000-000000000001"
	serviceCreateTestEnvironment = "20000000-0000-0000-0000-000000000001"
	serviceCreateTestService     = "30000000-0000-0000-0000-000000000001"
	serviceCreateTestVolume      = "40000000-0000-0000-0000-000000000001"
)

type serviceCreateTestProvider struct {
	RailwayProvider
	client graphql.Client
}

func (p *serviceCreateTestProvider) Configure(ctx context.Context, req frameworkprovider.ConfigureRequest, resp *frameworkprovider.ConfigureResponse) {
	resp.ResourceData = &p.client
}

type serviceCreateFixture struct {
	mu            sync.Mutex
	client        graphql.Client
	failOperation string
	serviceExists bool
	volumeExists  bool
	volumeName    string
	image         string
	creates       int
	deletes       int
}

func newServiceCreateFixture(t *testing.T, failOperation string) *serviceCreateFixture {
	t.Helper()
	f := &serviceCreateFixture{failOperation: failOperation}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		var request struct {
			Operation string                     `json:"operationName"`
			Variables map[string]json.RawMessage `json:"variables"`
		}
		if err := json.NewDecoder(req.Body).Decode(&request); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		respond := func(data interface{}) {
			if err := json.NewEncoder(w).Encode(map[string]interface{}{"data": data}); err != nil {
				t.Error(err)
			}
		}
		fail := func(message string) {
			if err := json.NewEncoder(w).Encode(map[string]interface{}{
				"errors": []map[string]string{{"message": message}},
			}); err != nil {
				t.Error(err)
			}
		}
		if request.Operation == f.failOperation {
			f.failOperation = ""
			fail("injected " + request.Operation + " failure")
			return
		}
		service := map[string]interface{}{
			"id": serviceCreateTestService, "name": "example", "projectId": serviceCreateTestProject,
		}
		volume := func() map[string]interface{} {
			return map[string]interface{}{
				"id": serviceCreateTestVolume, "name": f.volumeName,
				"volumeInstances": map[string]interface{}{"edges": []interface{}{
					map[string]interface{}{"node": map[string]interface{}{
						"id": "volume-instance", "environmentId": serviceCreateTestEnvironment,
						"serviceId": serviceCreateTestService, "mountPath": "/data", "sizeMB": 500,
					}},
				}},
			}
		}
		switch request.Operation {
		case "createService":
			if f.serviceExists {
				fail(`A service named "example" already exists in this project`)
				return
			}
			f.serviceExists = true
			f.image = ""
			f.creates++
			respond(map[string]interface{}{"serviceCreate": service})
		case "updateServiceInstance":
			respond(map[string]bool{"serviceInstanceUpdate": true})
		case "createVolume":
			if f.volumeExists {
				fail("previous volume was not cleaned up")
				return
			}
			f.volumeExists = true
			f.volumeName = "initial-volume-name"
			respond(map[string]interface{}{"volumeCreate": volume()})
		case "updateVolume":
			f.volumeName = "example-data"
			respond(map[string]interface{}{"volumeUpdate": volume()})
		case "connectService":
			f.image = "ghcr.io/example/app:v1"
			respond(map[string]interface{}{"serviceConnect": service})
		case "getService":
			respond(map[string]interface{}{"service": service})
		case "getProject":
			respond(map[string]interface{}{"project": map[string]interface{}{
				"id": serviceCreateTestProject, "name": "test", "description": "", "isPublic": false, "prDeploys": false,
				"environments": map[string]interface{}{"edges": []interface{}{
					map[string]interface{}{"node": map[string]interface{}{
						"id": serviceCreateTestEnvironment, "name": "production", "createdAt": "2024-01-01T00:00:00Z",
					}},
				}},
			}})
		case "getServiceInstance":
			var source interface{}
			if f.image != "" {
				source = map[string]string{"image": f.image}
			}
			respond(map[string]interface{}{"serviceInstance": map[string]interface{}{
				"source": source, "watchPatterns": []string{}, "restartPolicyType": "ON_FAILURE",
				"restartPolicyMaxRetries": 10, "sleepApplication": false,
			}})
		case "getVolumeInstances":
			edges := []interface{}{}
			if f.volumeExists {
				edges = append(edges, map[string]interface{}{"node": volume()})
			}
			respond(map[string]interface{}{"project": map[string]interface{}{
				"volumes": map[string]interface{}{"edges": edges},
			}})
		case "deleteService":
			f.serviceExists = false
			f.deletes++
			respond(map[string]bool{"serviceDelete": true})
		case "deleteVolume":
			var id string
			if err := json.Unmarshal(request.Variables["id"], &id); err != nil {
				t.Error(err)
			}
			if id != serviceCreateTestVolume {
				fail("volume identity was lost")
				return
			}
			f.volumeExists = false
			respond(map[string]bool{"volumeDelete": true})
		default:
			t.Errorf("unexpected GraphQL operation %q", request.Operation)
			fail("unexpected operation")
		}
	}))
	t.Cleanup(server.Close)
	f.client = graphql.NewClient(server.URL, server.Client())
	return f
}
