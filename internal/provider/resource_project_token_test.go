package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/Khan/genqlient/graphql"
	fwresource "github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

const (
	projectTokenTestProject     = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	projectTokenTestEnvironment = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	projectTokenTestSecret      = "mock-issued-credential-"
	projectTokenTestAddress     = "railway_project_token.test"
)

type projectTokenTestTransport func(*http.Request) (*http.Response, error)

func (f projectTokenTestTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

type projectTokenTestAPI struct {
	mu            sync.Mutex
	tokens        map[string]ProjectToken
	events        []string
	issued        int
	failIdentity  bool
	accountTokens map[string]bool
}

func (api *projectTokenTestAPI) roundTrip(t *testing.T, req *http.Request) (*http.Response, error) {
	api.mu.Lock()
	defer api.mu.Unlock()

	var call struct {
		Operation string `json:"operationName"`
		Variables struct {
			Input     ProjectTokenCreateInput
			ID        string
			ProjectID string
			After     *string
		}
	}
	if err := json.NewDecoder(req.Body).Decode(&call); err != nil {
		return nil, err
	}

	bearer, projectToken := req.Header.Get("Authorization"), req.Header.Get("Project-Access-Token")
	if call.Operation == "identifyProjectToken" {
		if bearer != "" || !strings.HasPrefix(projectToken, projectTokenTestSecret) {
			t.Error("identity lookup requires only the issued project credential")
		}
	} else if !api.accountTokens[bearer] || projectToken != "" {
		t.Errorf("%s requires only the provider credential", call.Operation)
	}

	switch call.Operation {
	case "createProject", "getProject":
		if call.Operation == "createProject" {
			api.events = append(api.events, "create project")
		}
		field := "project"
		if call.Operation == "createProject" {
			field = "projectCreate"
		}
		return projectTokenTestResponse(http.StatusOK, fmt.Sprintf(`{"data":{"%s":{"id":%q,"name":"project-token-test","description":"","isPublic":false,"prDeploys":false,"workspace":{"id":"eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee"},"environments":{"edges":[{"node":{"id":%q,"name":"production","createdAt":"2024-01-01T00:00:00Z"}}]}}}}`, field, projectTokenTestProject, projectTokenTestEnvironment))
	case "deleteProject":
		api.events = append(api.events, "delete project")
		return projectTokenTestResponse(http.StatusOK, `{"data":{"projectDelete":true}}`)
	case "createProjectToken":
		api.issued++
		id := fmt.Sprintf("record-%d", api.issued)
		api.events = append(api.events, "create "+id)
		api.tokens[id] = ProjectToken{
			Id: id, Name: call.Variables.Input.Name,
			ProjectId: call.Variables.Input.ProjectId, EnvironmentId: call.Variables.Input.EnvironmentId,
		}
		return projectTokenTestResponse(http.StatusOK, fmt.Sprintf(`{"data":{"projectTokenCreate":%q}}`, projectTokenTestSecret+id))
	case "identifyProjectToken":
		if api.failIdentity {
			api.failIdentity = false
			return projectTokenTestResponse(http.StatusOK, fmt.Sprintf(`{"errors":[{"message":%q}]}`, projectToken))
		}
		token := api.tokens[strings.TrimPrefix(projectToken, projectTokenTestSecret)]
		metadata, err := json.Marshal(token)
		if err != nil {
			return nil, err
		}
		return projectTokenTestResponse(http.StatusOK, fmt.Sprintf(`{"data":{"projectToken":%s}}`, metadata))
	case "getProjectTokens":
		// An unrelated token with the same name appears on the first page.
		if call.Variables.After == nil {
			return projectTokenTestResponse(http.StatusOK, fmt.Sprintf(`{"data":{"projectTokens":{"edges":[{"node":{"id":"unrelated","name":"CI","projectId":%q,"environmentId":%q}}],"pageInfo":{"hasNextPage":true,"endCursor":"cursor"}}}}`, call.Variables.ProjectID, projectTokenTestEnvironment))
		}
		if *call.Variables.After != "cursor" {
			return nil, errors.New("incorrect pagination cursor")
		}
		edges := make([]map[string]ProjectToken, 0)
		for _, token := range api.tokens {
			if token.ProjectId == call.Variables.ProjectID {
				edges = append(edges, map[string]ProjectToken{"node": token})
			}
		}
		metadata, err := json.Marshal(edges)
		if err != nil {
			return nil, err
		}
		return projectTokenTestResponse(http.StatusOK, fmt.Sprintf(`{"data":{"projectTokens":{"edges":%s,"pageInfo":{"hasNextPage":false}}}}`, metadata))
	case "deleteProjectToken":
		id := call.Variables.ID
		_, found := api.tokens[id]
		if !found {
			t.Error("revocation did not use a known token record ID")
		}
		api.events = append(api.events, "delete "+id)
		delete(api.tokens, id)
		return projectTokenTestResponse(http.StatusOK, fmt.Sprintf(`{"data":{"projectTokenDelete":%t}}`, found))
	default:
		return nil, fmt.Errorf("unexpected GraphQL operation %q", call.Operation)
	}
}

func projectTokenTestResponse(status int, body string) (*http.Response, error) {
	return &http.Response{StatusCode: status, Status: http.StatusText(status), Body: io.NopCloser(strings.NewReader(body))}, nil
}

func mockProjectTokenAPI(t *testing.T) *projectTokenTestAPI {
	t.Helper()
	api := &projectTokenTestAPI{
		tokens:        make(map[string]ProjectToken),
		accountTokens: map[string]bool{"Bearer dummy-provider-credential": true, "Bearer rotated-provider-credential": true},
	}
	t.Setenv("RAILWAY_TOKEN", "dummy-provider-credential")
	t.Setenv("HTTP_DEBUG", "1")

	originalTransport, originalStderr := http.DefaultTransport, os.Stderr
	log, err := os.CreateTemp(t.TempDir(), "http-debug")
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = log
	http.DefaultTransport = projectTokenTestTransport(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host != "backboard.railway.app" {
			return originalTransport.RoundTrip(req)
		}
		return api.roundTrip(t, req)
	})
	t.Cleanup(func() {
		http.DefaultTransport, os.Stderr = originalTransport, originalStderr
		log.Close()
		output, err := os.ReadFile(log.Name())
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(output), "Railway request:") && len(api.events) > 2 {
			t.Error("HTTP debugging was not active")
		}
		for _, credential := range []string{projectTokenTestSecret, "dummy-provider-credential", "rotated-provider-credential"} {
			if strings.Contains(string(output), credential) {
				t.Error("HTTP debug logs exposed a credential")
			}
		}
	})
	return api
}

const projectTokenTestProjectConfig = `
resource "railway_project" "test" {
  name = "project-token-test"
  default_environment = {
    name = "production"
  }
}
`

func projectTokenTestConfig(name, project, environment string) string {
	return projectTokenTestProjectConfig + fmt.Sprintf(`
resource "railway_project_token" "test" {
  name           = %q
  project_id     = %s
  environment_id = %s

  lifecycle {
    create_before_destroy = true
  }
}
resource "terraform_data" "consumer" {
  triggers_replace = railway_project_token.test.token
}
output "token" {
  value     = railway_project_token.test.token
  sensitive = true
}
`, name, project, environment)
}

type projectTokenPlanCheck struct {
	unknown bool
}

func (check projectTokenPlanCheck) CheckPlan(_ context.Context, req plancheck.CheckPlanRequest, resp *plancheck.CheckPlanResponse) {
	for _, change := range req.Plan.ResourceChanges {
		if change.Address != projectTokenTestAddress {
			continue
		}
		c := change.Change
		if check.unknown {
			unknown, _ := c.AfterUnknown.(map[string]interface{})
			sensitive, _ := c.AfterSensitive.(map[string]interface{})
			if unknown["token"] != true || sensitive["token"] != true {
				resp.Error = errors.New("planned credential must be sensitive and unknown")
			}
		} else {
			sensitive, _ := c.BeforeSensitive.(map[string]interface{})
			if sensitive["token"] != true || c.After != nil {
				resp.Error = errors.New("destroy plan must mark the prior credential sensitive")
			}
		}
		return
	}
	resp.Error = errors.New("project token change missing from plan")
}

func TestProjectTokenResource(t *testing.T) {
	api := mockProjectTokenAPI(t)
	project, environment := "railway_project.test.id", "railway_project.test.default_environment.id"
	base := projectTokenTestConfig("CI", project, environment)
	renamed := projectTokenTestConfig("New CI", project, environment)
	newEnvironment := fmt.Sprintf("%q", "cccccccc-cccc-cccc-cccc-cccccccccccc")
	newProject := fmt.Sprintf("%q", "dddddddd-dddd-dddd-dddd-dddddddddddd")
	movedEnvironment := projectTokenTestConfig("New CI", project, newEnvironment)
	movedProject := projectTokenTestConfig("New CI", newProject, newEnvironment)

	checkState := func(record, projectID, environmentID string) resource.TestCheckFunc {
		return resource.ComposeAggregateTestCheckFunc(
			resource.TestCheckResourceAttr(projectTokenTestAddress, "id", record),
			resource.TestCheckResourceAttr(projectTokenTestAddress, "project_id", projectID),
			resource.TestCheckResourceAttr(projectTokenTestAddress, "environment_id", environmentID),
			resource.TestCheckResourceAttr("terraform_data.consumer", "triggers_replace", projectTokenTestSecret+record),
			resource.TestCheckOutput("token", projectTokenTestSecret+record),
		)
	}
	plan := func(action plancheck.ResourceActionType) resource.ConfigPlanChecks {
		return resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
			plancheck.ExpectResourceAction(projectTokenTestAddress, action),
			projectTokenPlanCheck{unknown: action != plancheck.ResourceActionDestroy},
		}}
	}
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{Config: base, Check: checkState("record-1", projectTokenTestProject, projectTokenTestEnvironment), ConfigPlanChecks: plan(plancheck.ResourceActionCreate)},
			{
				PreConfig: func() { t.Setenv("RAILWAY_TOKEN", "rotated-provider-credential") },
				Config:    base, Check: checkState("record-1", projectTokenTestProject, projectTokenTestEnvironment),
			},
			{Config: renamed, Check: checkState("record-2", projectTokenTestProject, projectTokenTestEnvironment), ConfigPlanChecks: plan(plancheck.ResourceActionReplace)},
			{Config: movedEnvironment, ConfigPlanChecks: plan(plancheck.ResourceActionReplace)},
			{Config: movedProject, ConfigPlanChecks: plan(plancheck.ResourceActionReplace)},
			{
				PreConfig: func() {
					api.mu.Lock()
					defer api.mu.Unlock()
					delete(api.tokens, "record-4")
				},
				Config: movedProject, ConfigPlanChecks: plan(plancheck.ResourceActionCreate),
			},
			{Config: movedProject, Destroy: true, ConfigPlanChecks: plan(plancheck.ResourceActionDestroy)},
		},
	})
	want := []string{
		"create project", "create record-1",
		"create record-2", "delete record-1", "create record-3", "delete record-2",
		"create record-4", "delete record-3", "create record-5", "delete record-5", "delete project",
	}
	if !reflect.DeepEqual(api.events, want) || len(api.tokens) != 0 {
		t.Fatalf("unexpected token lifecycle: %v", api.events)
	}
}

func TestProjectTokenResourcePartialCreate(t *testing.T) {
	api := mockProjectTokenAPI(t)
	api.failIdentity = true
	ctx := context.Background()
	client := graphql.NewClient(railwayEndpoint, &http.Client{
		Transport: &authedTransport{token: "dummy-provider-credential", wrapped: http.DefaultTransport},
	})
	r := ProjectTokenResource{client: &client}
	var schema fwresource.SchemaResponse
	r.Schema(ctx, fwresource.SchemaRequest{}, &schema)
	plan := tfsdk.Plan{Schema: schema.Schema}
	if plan.Set(ctx, &ProjectTokenResourceModel{
		Name:          types.StringValue("CI"),
		ProjectId:     types.StringValue(projectTokenTestProject),
		EnvironmentId: types.StringValue(projectTokenTestEnvironment),
		Id:            types.StringUnknown(),
		Token:         types.StringUnknown(),
	}).HasError() {
		t.Fatal("could not set token plan")
	}
	created := fwresource.CreateResponse{State: tfsdk.State{Schema: schema.Schema}}
	r.Create(ctx, fwresource.CreateRequest{Plan: plan}, &created)
	var partial ProjectTokenResourceModel
	if !created.Diagnostics.HasError() || created.State.Get(ctx, &partial).HasError() ||
		!partial.Id.IsNull() || partial.Token.ValueString() != projectTokenTestSecret+"record-1" {
		t.Fatal("failed identity lookup must save the issued credential for cleanup")
	}
	for _, d := range created.Diagnostics {
		if strings.Contains(d.Summary()+d.Detail(), projectTokenTestSecret) {
			t.Error("create diagnostic exposed the credential")
		}
	}
	deleted := fwresource.DeleteResponse{}
	r.Delete(ctx, fwresource.DeleteRequest{State: created.State}, &deleted)
	if deleted.Diagnostics.HasError() {
		t.Fatal("could not revoke from partial state")
	}
	if !reflect.DeepEqual(api.events, []string{"create record-1", "delete record-1"}) || len(api.tokens) != 0 {
		t.Fatalf("partial create was not recovered during destroy: %v", api.events)
	}
}

func TestProjectTokenAPIErrors(t *testing.T) {
	for _, tc := range []struct {
		name       string
		status     int
		body       string
		wantAbsent bool
	}{
		{"missing project", 200, `{"errors":[{"message":"Project not found","extensions":{"code":"NOT_FOUND"}}]}`, true},
		{"permission denied", 200, `{"errors":[{"message":"Project not found","extensions":{"code":"FORBIDDEN"}}]}`, false},
		{"unauthenticated", 401, projectTokenTestSecret, false},
		{"network failure", 0, "", false},
		{"GraphQL failure", 200, fmt.Sprintf(`{"errors":[{"message":%q}]}`, projectTokenTestSecret), false},
		{"incomplete response", 200, `{"data":{"projectTokens":null}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := graphql.NewClient(railwayEndpoint, &http.Client{Transport: projectTokenTestTransport(func(req *http.Request) (*http.Response, error) {
				if tc.status == 0 {
					return nil, errors.New(projectTokenTestSecret)
				}
				return projectTokenTestResponse(tc.status, tc.body)
			})})
			r := ProjectTokenResource{client: &client}
			token, err := r.find(context.Background(), projectTokenTestProject, "record-1")
			if token != nil || (err == nil) != tc.wantAbsent {
				t.Fatal("only confirmed absence should be reported without an error")
			}
			if err != nil && strings.Contains(err.Error(), projectTokenTestSecret) {
				t.Error("diagnostic would expose a credential")
			}
		})
	}
}

func TestProjectTokenRevocationErrors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"unconfirmed revocation", 200, `{"data":{"projectTokenDelete":false}}`},
		{"GraphQL error despite true result", 200, fmt.Sprintf(`{"data":{"projectTokenDelete":true},"errors":[{"message":%q}]}`, projectTokenTestSecret)},
		{"permission denied", 403, projectTokenTestSecret},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := graphql.NewClient(railwayEndpoint, &http.Client{Transport: projectTokenTestTransport(func(req *http.Request) (*http.Response, error) {
				return projectTokenTestResponse(tc.status, tc.body)
			})})
			r := ProjectTokenResource{client: &client}
			err := r.revoke(context.Background(), "record-1")
			if err == nil || strings.Contains(err.Error(), projectTokenTestSecret) {
				t.Fatal("failed revocation must return a credential-free error")
			}
		})
	}
}

func TestAccProjectTokenResource(t *testing.T) {
	workspace, credential := os.Getenv("RAILWAY_PROJECT_TOKEN_TEST_WORKSPACE_ID"), os.Getenv("RAILWAY_PROJECT_TOKEN_TEST_API_TOKEN")
	if os.Getenv("TF_ACC") == "" || workspace == "" || credential == "" {
		t.Skip("set TF_ACC=1, RAILWAY_PROJECT_TOKEN_TEST_API_TOKEN, and RAILWAY_PROJECT_TOKEN_TEST_WORKSPACE_ID to run")
	}
	t.Setenv("RAILWAY_TOKEN", credential)
	name := resource.PrefixedUniqueId("terraform-project-token-")
	project := fmt.Sprintf(`
resource "railway_project" "test" {
  name         = %q
  workspace_id = %q
}
`, name, workspace)
	var projectID, tokenID string
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: project + `
resource "railway_project_token" "test" {
  name           = "Terraform acceptance test"
  project_id     = railway_project.test.id
  environment_id = railway_project.test.default_environment.id
}
`,
				Check: func(state *terraform.State) error {
					attributes := state.RootModule().Resources[projectTokenTestAddress].Primary.Attributes
					projectID, tokenID = attributes["project_id"], attributes["id"]
					if tokenID == "" || attributes["token"] == "" || tokenID == attributes["token"] {
						return errors.New("expected separate credential and record ID")
					}
					return nil
				},
			},
			{
				Config: project,
				Check: func(*terraform.State) error {
					client := graphql.NewClient(railwayEndpoint, &http.Client{Transport: &authedTransport{token: credential, wrapped: http.DefaultTransport}})
					r := ProjectTokenResource{client: &client}
					ctx := context.WithValue(context.Background(), projectTokenRequestKey{}, true)
					token, err := r.find(ctx, projectID, tokenID)
					if err == nil && token != nil {
						return errors.New("live project token still exists after destroy")
					}
					return err
				},
			},
		},
	})
}
