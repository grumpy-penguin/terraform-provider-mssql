package provider

import (
	"context"
	"fmt"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/grumpy-penguin/terraform-provider-mssql/internal/sqlclient"
)

var (
	_ resource.Resource                = &UserResource{}
	_ resource.ResourceWithConfigure   = &UserResource{}
	_ resource.ResourceWithImportState = &UserResource{}
)

// NewUserResource is the constructor registered with the provider.
func NewUserResource() resource.Resource {
	return &UserResource{}
}

// UserResource manages the mapping of a single Azure AD identity to an
// Azure SQL Database user, plus that user's database role memberships.
type UserResource struct {
	cache *clientCache

	defaultServer   string
	defaultPort     int
	defaultDatabase string
}

// userResourceModel is the Terraform-facing schema for mssql_user.
type userResourceModel struct {
	ID       types.String `tfsdk:"id"`
	UserName types.String `tfsdk:"user_name"`
	Server   types.String `tfsdk:"server"`
	Database types.String `tfsdk:"database"`
	Roles    types.Set    `tfsdk:"roles"`
}

func (r *UserResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_user"
}

func (r *UserResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Maps an Azure AD user or group to an Azure SQL Database user (CREATE USER ... FROM EXTERNAL PROVIDER) and manages the set of database roles it is a member of. Roles must already exist in the database; this resource does not create them (create custom ones with mssql_role).",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:      true,
				Description:   "\"<server>/<database>/<user_name>\" — unique across every server/database this provider manages.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"user_name": schema.StringAttribute{
				Required:    true,
				Description: "The Azure AD identity to map, and the resulting database user name. Typically the AAD user's UPN (e.g. jane.doe@contoso.com) or an AAD group's display name. Changing this forces replacement.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"server": schema.StringAttribute{
				Optional: true,
				Description: "Azure SQL logical server FQDN to connect to for this mapping, e.g. myserver.database.windows.net. Overrides the provider's server, so a single mssql provider can manage users across multiple servers — " +
					"including one created in this same apply by another provider, since a resource (unlike a provider block) can safely reference another resource's computed attributes. Falls back to the provider's server if unset; one of the two must be set. Changing this forces replacement.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"database": schema.StringAttribute{
				Optional:    true,
				Description: "Target database name for this mapping. Overrides the provider's database. Falls back to the provider's database if unset; one of the two must be set. Changing this forces replacement.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"roles": schema.SetAttribute{
				Required:    true,
				ElementType: types.StringType,
				Description: "Database roles this user should be a member of, e.g. [\"db_datareader\", \"db_datawriter\"]. Reconciled exactly: roles removed from this list are revoked from the user. Roles must already exist in the database; reference an mssql_role's name here so Terraform creates the role first.",
			},
		},
	}
}

func (r *UserResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	data, ok := req.ProviderData.(*providerData)
	if !ok {
		resp.Diagnostics.AddError("Unexpected Resource Configure Type", fmt.Sprintf("Expected *provider.providerData, got: %T", req.ProviderData))
		return
	}
	r.cache = data.cache
	r.defaultServer = data.defaultServer
	r.defaultPort = data.defaultPort
	r.defaultDatabase = data.defaultDatabase
}

// resolveTarget determines which server/database this resource instance
// should connect to: its own server/database if set, otherwise the
// provider's defaults. Returns an error if neither source supplies one.
func (r *UserResource) resolveTarget(model userResourceModel) (sqlclient.Target, error) {
	return resolveTarget("mssql_user", model.Server.ValueString(), model.Database.ValueString(), r.defaultServer, r.defaultPort, r.defaultDatabase)
}

func (r *UserResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan userResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	target, err := r.resolveTarget(plan)
	if err != nil {
		resp.Diagnostics.AddError("Unable to determine connection target", err.Error())
		return
	}
	client, err := r.cache.Get(ctx, target)
	if err != nil {
		resp.Diagnostics.AddError("Unable to connect to Azure SQL Database", err.Error())
		return
	}

	userName := plan.UserName.ValueString()
	desiredRoles := stringSetToSlice(ctx, plan.Roles, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}

	if err := client.EnsureExternalUser(ctx, userName); err != nil {
		resp.Diagnostics.AddError("Unable to create database user", err.Error())
		return
	}

	if _, _, err := client.SyncRoles(ctx, userName, desiredRoles); err != nil {
		resp.Diagnostics.AddError("Unable to assign database roles", err.Error())
		return
	}

	if err := r.readInto(ctx, client, target, userName, &plan); err != nil {
		resp.Diagnostics.AddError("Unable to read back created user", err.Error())
		return
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *UserResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state userResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	target, err := r.resolveTarget(state)
	if err != nil {
		resp.Diagnostics.AddError("Unable to determine connection target", err.Error())
		return
	}
	client, err := r.cache.Get(ctx, target)
	if err != nil {
		resp.Diagnostics.AddError("Unable to connect to Azure SQL Database", err.Error())
		return
	}

	userName := state.UserName.ValueString()
	info, err := client.GetPrincipal(ctx, userName)
	if err != nil {
		resp.Diagnostics.AddError("Unable to read database user", err.Error())
		return
	}
	if !info.Exists {
		resp.State.RemoveResource(ctx)
		return
	}

	if err := r.readInto(ctx, client, target, userName, &state); err != nil {
		resp.Diagnostics.AddError("Unable to read database user roles", err.Error())
		return
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *UserResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan userResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	target, err := r.resolveTarget(plan)
	if err != nil {
		resp.Diagnostics.AddError("Unable to determine connection target", err.Error())
		return
	}
	client, err := r.cache.Get(ctx, target)
	if err != nil {
		resp.Diagnostics.AddError("Unable to connect to Azure SQL Database", err.Error())
		return
	}

	userName := plan.UserName.ValueString()
	desiredRoles := stringSetToSlice(ctx, plan.Roles, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}

	if err := client.EnsureExternalUser(ctx, userName); err != nil {
		resp.Diagnostics.AddError("Unable to ensure database user exists", err.Error())
		return
	}

	if _, _, err := client.SyncRoles(ctx, userName, desiredRoles); err != nil {
		resp.Diagnostics.AddError("Unable to reconcile database roles", err.Error())
		return
	}

	if err := r.readInto(ctx, client, target, userName, &plan); err != nil {
		resp.Diagnostics.AddError("Unable to read back updated user", err.Error())
		return
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *UserResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state userResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	target, err := r.resolveTarget(state)
	if err != nil {
		resp.Diagnostics.AddError("Unable to determine connection target", err.Error())
		return
	}
	client, err := r.cache.Get(ctx, target)
	if err != nil {
		resp.Diagnostics.AddError("Unable to connect to Azure SQL Database", err.Error())
		return
	}

	if err := client.DropUser(ctx, state.UserName.ValueString()); err != nil {
		resp.Diagnostics.AddError("Unable to delete database user", err.Error())
	}
}

// ImportState accepts "<server>/<database>/<user_name>", the only way to
// unambiguously identify a mapping now that server/database can vary per
// resource instance instead of being fixed by the provider.
func (r *UserResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	parts := strings.SplitN(req.ID, "/", 3)
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		resp.Diagnostics.AddError(
			"Unexpected Import Identifier",
			`Expected an import ID in the form "<server>/<database>/<user_name>", e.g. "myserver.database.windows.net/mydatabase/jane.doe@contoso.com".`,
		)
		return
	}
	server, database, userName := parts[0], parts[1], parts[2]

	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("server"), server)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("database"), database)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("user_name"), userName)...)
}

// readInto populates model's id/roles from current database state for
// userName at target.
func (r *UserResource) readInto(ctx context.Context, client *sqlclient.Client, target sqlclient.Target, userName string, model *userResourceModel) error {
	roles, err := client.CurrentRoles(ctx, userName)
	if err != nil {
		return err
	}

	model.ID = types.StringValue(fmt.Sprintf("%s/%s/%s", target.Server, target.Database, userName))
	model.UserName = types.StringValue(userName)
	model.Roles = stringSliceToSet(roles)
	return nil
}
