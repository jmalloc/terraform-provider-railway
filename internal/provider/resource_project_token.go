package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/Khan/genqlient/graphql"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/vektah/gqlparser/v2/gqlerror"
)

var _ resource.Resource = &ProjectTokenResource{}

func NewProjectTokenResource() resource.Resource {
	return &ProjectTokenResource{}
}

type ProjectTokenResource struct {
	client *graphql.Client
}

type ProjectTokenResourceModel struct {
	Id            types.String `tfsdk:"id"`
	Name          types.String `tfsdk:"name"`
	ProjectId     types.String `tfsdk:"project_id"`
	EnvironmentId types.String `tfsdk:"environment_id"`
	Token         types.String `tfsdk:"token"`
}

func (r *ProjectTokenResource) Metadata(ctx context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_project_token"
}

func (r *ProjectTokenResource) Schema(ctx context.Context, req resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Railway project token scoped to one environment. Use an account or workspace API token to authenticate the provider. " +
			"Changing the name, project, or environment replaces the token. Import is unsupported because Railway only returns the credential when it is issued.\n\n" +
			"Terraform redacts the sensitive credential in normal output. Mark any downstream outputs that expose the credential as sensitive. State and saved plans can still contain it, so protect those artifacts with your backend and access controls.\n\n" +
			"If issuance succeeds but identity lookup fails, the provider saves the credential in partial state. " +
			"After API access is restored, destroy the failed token resource before applying again. " +
			"If the issuance response was lost or the API still cannot identify the token, check the project's token settings and revoke the issued token before retrying.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				MarkdownDescription: "Identifier of the project token record.",
				Computed:            true,
			},
			"name": schema.StringAttribute{
				MarkdownDescription: "Name of the project token.",
				Required:            true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
				Validators: []validator.String{
					stringvalidator.LengthAtLeast(1),
				},
			},
			"project_id": schema.StringAttribute{
				MarkdownDescription: "Identifier of the project the token belongs to.",
				Required:            true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
				Validators: []validator.String{
					stringvalidator.RegexMatches(uuidRegex(), "must be an id"),
				},
			},
			"environment_id": schema.StringAttribute{
				MarkdownDescription: "Identifier of the environment the token belongs to.",
				Required:            true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
				Validators: []validator.String{
					stringvalidator.RegexMatches(uuidRegex(), "must be an id"),
				},
			},
			"token": schema.StringAttribute{
				MarkdownDescription: "Project token credential issued by Railway.",
				Computed:            true,
				Sensitive:           true,
			},
		},
	}
}

func (r *ProjectTokenResource) Configure(ctx context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}

	client, ok := req.ProviderData.(*graphql.Client)
	if !ok {
		resp.Diagnostics.AddError(
			"Unexpected Resource Configure Type",
			fmt.Sprintf("Expected *graphql.Client, got: %T. Please report this issue to the provider developers.", req.ProviderData),
		)
		return
	}

	r.client = client
}

func (r *ProjectTokenResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var data ProjectTokenResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	response, err := createProjectToken(ctx, *r.client, ProjectTokenCreateInput{
		Name:          data.Name.ValueString(),
		ProjectId:     data.ProjectId.ValueString(),
		EnvironmentId: data.EnvironmentId.ValueString(),
	})
	if response.ProjectTokenCreate == "" {
		resp.Diagnostics.AddError("Unable to issue project token", "Railway did not return a credential. Check the project's token settings before retrying.")
		return
	}

	data.Token = types.StringValue(response.ProjectTokenCreate)
	data.Id = types.StringNull()

	// Keep enough state to retry identity lookup or revoke the exact token.
	// A failed lookup must never trigger a second create request.
	identityErr := data.resolveID(ctx)
	stateDiags := resp.State.Set(ctx, &data)
	if stateDiags.HasError() {
		if data.Id.IsNull() || r.revoke(ctx, data.Id.ValueString()) != nil {
			resp.Diagnostics.AddError("Unable to save or revoke project token", "Check the project's token settings and revoke the issued token before retrying.")
			return
		}
		resp.Diagnostics.AddError("Unable to save project token", "The issued token was revoked.")
		return
	}
	if err != nil || identityErr != nil {
		resp.Diagnostics.AddError("Unable to complete project token creation", "The issued credential was saved in partial state. Restore API access and destroy the failed token resource before applying again.")
	}
}

func (r *ProjectTokenResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var data ProjectTokenResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if data.Id.IsNull() {
		if err := data.resolveID(ctx); err != nil {
			resp.Diagnostics.AddError("Unable to identify issued project token", err.Error())
			return
		}
	}
	token, err := r.find(ctx, data.ProjectId.ValueString(), data.Id.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Unable to read project token", err.Error())
		return
	}
	if token == nil {
		resp.State.RemoveResource(ctx)
		return
	}

	if !strings.EqualFold(token.ProjectId, data.ProjectId.ValueString()) ||
		!strings.EqualFold(token.EnvironmentId, data.EnvironmentId.ValueString()) {
		resp.Diagnostics.AddError("Unable to read project token", "Railway returned a token record for an unexpected project or environment.")
		return
	}
	data.Name = types.StringValue(token.Name)
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

func (r *ProjectTokenResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	resp.Diagnostics.AddError("Unable to update project token", "Project tokens can only be replaced.")
}

func (r *ProjectTokenResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var data ProjectTokenResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if data.Id.IsNull() {
		if err := data.resolveID(ctx); err != nil {
			resp.Diagnostics.AddError("Unable to identify issued project token", err.Error())
			return
		}
	}
	projectID, id := data.ProjectId.ValueString(), data.Id.ValueString()
	token, err := r.find(ctx, projectID, id)
	if err != nil {
		resp.Diagnostics.AddError("Unable to check project token before revocation", err.Error())
		return
	}
	if token == nil {
		return
	}

	if err := r.revoke(ctx, id); err != nil {
		if errors.Is(err, errProjectTokenDeleteUnconfirmed) {
			// The token may have been revoked between the list and mutation.
			if token, listErr := r.find(ctx, projectID, id); token == nil && listErr == nil {
				return
			}
		}
		resp.Diagnostics.AddError("Unable to revoke project token", err.Error())
	}
}

type projectTokenHTTPClient struct {
	client *http.Client
	token  string
}

func (c *projectTokenHTTPClient) Do(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Set("Project-Access-Token", c.token)
	return c.client.Do(req)
}

func (data *ProjectTokenResourceModel) resolveID(ctx context.Context) error {
	credential := data.Token.ValueString()
	if credential == "" {
		return errors.New("The issued credential is unavailable.")
	}

	client := graphql.NewClient(railwayEndpoint, &projectTokenHTTPClient{
		token: credential,
		client: &http.Client{
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	})
	response, err := identifyProjectToken(ctx, client)
	if err != nil {
		return errors.New("Railway could not identify the issued credential.")
	}

	token := response.ProjectToken
	if token.Id == "" || token.Id == credential ||
		!strings.EqualFold(token.ProjectId, data.ProjectId.ValueString()) ||
		!strings.EqualFold(token.EnvironmentId, data.EnvironmentId.ValueString()) {
		return errors.New("Railway returned an invalid token record or an unexpected project or environment.")
	}
	data.Id = types.StringValue(token.Id)
	return nil
}

func (r *ProjectTokenResource) find(ctx context.Context, projectID, id string) (*ProjectToken, error) {
	seen := map[string]bool{}
	var after *string
	for {
		response, err := getProjectTokens(ctx, *r.client, projectID, after)
		if err != nil {
			var graphQLErrors gqlerror.List
			if errors.As(err, &graphQLErrors) && len(graphQLErrors) == 1 && graphQLErrors[0] != nil {
				apiErr := graphQLErrors[0]
				code := apiErr.Extensions["code"]
				if apiErr.Message == "Project not found" && (code == nil || code == "NOT_FOUND") {
					return nil, nil
				}
			}
			return nil, errors.New("Railway could not list the project's tokens. Check API availability and the provider credential's access to the project.")
		}
		page := response.ProjectTokens
		if page == nil || page.Edges == nil || page.PageInfo == nil {
			return nil, errors.New("Railway returned an incomplete project token list.")
		}
		for _, edge := range page.Edges {
			if edge.Node.Id == id {
				return &edge.Node.ProjectToken, nil
			}
		}
		if !page.PageInfo.HasNextPage {
			return nil, nil
		}
		after = page.PageInfo.EndCursor
		if after == nil || *after == "" || seen[*after] {
			return nil, errors.New("Railway returned an invalid project token pagination cursor.")
		}
		seen[*after] = true
	}
}

var errProjectTokenDeleteUnconfirmed = errors.New("Railway did not confirm token revocation.")

func (r *ProjectTokenResource) revoke(ctx context.Context, id string) error {
	response, err := deleteProjectToken(ctx, *r.client, id)
	if err != nil {
		return errors.New("Railway returned an error while revoking the token.")
	}
	if !response.ProjectTokenDelete {
		return errProjectTokenDeleteUnconfirmed
	}
	return nil
}
