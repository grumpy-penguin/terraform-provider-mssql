package provider

import (
	"context"
	"fmt"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/setdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-framework/types/basetypes"

	"github.com/grumpy-penguin/terraform-provider-mssql/internal/sqlclient"
)

var (
	_ resource.Resource                   = &RoleResource{}
	_ resource.ResourceWithConfigure      = &RoleResource{}
	_ resource.ResourceWithImportState    = &RoleResource{}
	_ resource.ResourceWithValidateConfig = &RoleResource{}
)

// NewRoleResource is the constructor registered with the provider.
func NewRoleResource() resource.Resource {
	return &RoleResource{}
}

// RoleResource manages a custom database role and the exact set of
// permissions granted to it, so roles like a db_executor can be created
// alongside the mssql_user resources that are members of them.
type RoleResource struct {
	cache *clientCache

	defaultServer   string
	defaultPort     int
	defaultDatabase string
}

// roleResourceModel is the Terraform-facing schema for mssql_role.
type roleResourceModel struct {
	ID          types.String `tfsdk:"id"`
	Name        types.String `tfsdk:"name"`
	Server      types.String `tfsdk:"server"`
	Database    types.String `tfsdk:"database"`
	Permissions types.Set    `tfsdk:"permissions"`
}

// rolePermissionModel is one element of mssql_role.permissions.
type rolePermissionModel struct {
	Permission types.String `tfsdk:"permission"`
	Class      types.String `tfsdk:"class"`
	Securable  types.String `tfsdk:"securable"`
}

var rolePermissionAttrTypes = map[string]attr.Type{
	"permission": types.StringType,
	"class":      types.StringType,
	"securable":  types.StringType,
}

var rolePermissionObjectType = types.ObjectType{AttrTypes: rolePermissionAttrTypes}

func (r *RoleResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_role"
}

func (r *RoleResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Creates a custom Azure SQL Database role (CREATE ROLE) and manages the exact set of permissions granted to it, e.g. EXECUTE on a schema for a stored-procedure-only application identity. " +
			"Add users to the role with mssql_user's roles list. An existing custom role of the same name is adopted; fixed roles (db_owner, db_datareader, ...) and public can't be managed.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:      true,
				Description:   "\"<server>/<database>/<name>\" — unique across every server/database this provider manages.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"name": schema.StringAttribute{
				Required:    true,
				Description: "Name of the database role, e.g. db_executor. Changing this forces replacement.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"server": schema.StringAttribute{
				Optional:    true,
				Description: "Azure SQL logical server FQDN to connect to, e.g. myserver.database.windows.net. Overrides the provider's server; one of the two must be set. Changing this forces replacement.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"database": schema.StringAttribute{
				Optional:    true,
				Description: "Target database name. Overrides the provider's database; one of the two must be set. Changing this forces replacement.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"permissions": schema.SetNestedAttribute{
				Optional: true,
				Computed: true,
				Default:  setdefault.StaticValue(types.SetValueMust(rolePermissionObjectType, []attr.Value{})),
				Description: "Permissions granted to the role. Reconciled exactly: permissions removed from this set are revoked. " +
					"Only GRANTs on the DATABASE, SCHEMA, OBJECT and TYPE classes are managed; DENYs and column-level grants are left alone.",
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"permission": schema.StringAttribute{
							Required:    true,
							Description: "Upper-case T-SQL permission name, e.g. EXECUTE, SELECT or VIEW DEFINITION.",
						},
						"class": schema.StringAttribute{
							Required:    true,
							Description: "Securable class the permission applies to: DATABASE, SCHEMA, OBJECT or TYPE.",
						},
						"securable": schema.StringAttribute{
							Optional:    true,
							Description: "The securable: omit for DATABASE, a schema name for SCHEMA (e.g. dbo), or \"<schema>.<name>\" for OBJECT and TYPE (e.g. dbo.PhoneEntryIdTVP).",
						},
					},
				},
			},
		},
	}
}

// ValidateConfig catches malformed role names and permissions at plan
// time instead of partway through an apply.
func (r *RoleResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var config roleResourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if !config.Name.IsUnknown() && !config.Name.IsNull() {
		if err := sqlclient.ValidateRoleName(config.Name.ValueString()); err != nil {
			resp.Diagnostics.AddAttributeError(path.Root("name"), "Invalid role name", err.Error())
		}
	}

	if config.Permissions.IsUnknown() || config.Permissions.IsNull() {
		return
	}
	// Walk elements individually rather than ElementsAs the whole set: an
	// element built from not-yet-known values is itself unknown at plan
	// time and can't be converted to a struct; it's checked again on a
	// later plan once known.
	for _, element := range config.Permissions.Elements() {
		obj, ok := element.(types.Object)
		if !ok || obj.IsUnknown() || obj.IsNull() {
			continue
		}
		var e rolePermissionModel
		if d := obj.As(ctx, &e, basetypes.ObjectAsOptions{}); d.HasError() {
			resp.Diagnostics.Append(d...)
			return
		}
		if e.Permission.IsUnknown() || e.Class.IsUnknown() || e.Securable.IsUnknown() {
			continue
		}
		p := sqlclient.Permission{
			Permission: e.Permission.ValueString(),
			Class:      e.Class.ValueString(),
			Securable:  e.Securable.ValueString(),
		}
		if err := sqlclient.ValidatePermission(p); err != nil {
			resp.Diagnostics.AddAttributeError(path.Root("permissions"), "Invalid permission", err.Error())
		}
	}
}

func (r *RoleResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

func (r *RoleResource) resolveTarget(model roleResourceModel) (sqlclient.Target, error) {
	return resolveTarget("mssql_role", model.Server.ValueString(), model.Database.ValueString(), r.defaultServer, r.defaultPort, r.defaultDatabase)
}

// connect resolves model's target and returns a client for it, reporting
// any failure into diags.
func (r *RoleResource) connect(ctx context.Context, model roleResourceModel, diags *diag.Diagnostics) (*sqlclient.Client, sqlclient.Target, bool) {
	target, err := r.resolveTarget(model)
	if err != nil {
		diags.AddError("Unable to determine connection target", err.Error())
		return nil, target, false
	}
	client, err := r.cache.Get(ctx, target)
	if err != nil {
		diags.AddError("Unable to connect to Azure SQL Database", err.Error())
		return nil, target, false
	}
	return client, target, true
}

func (r *RoleResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan roleResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	r.apply(ctx, plan, &resp.State, &resp.Diagnostics)
}

func (r *RoleResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan roleResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	r.apply(ctx, plan, &resp.State, &resp.Diagnostics)
}

// apply is shared by Create and Update: both ensure the role exists,
// reconcile its permissions to plan, then record what the database now
// reports.
func (r *RoleResource) apply(ctx context.Context, plan roleResourceModel, state *tfsdk.State, diags *diag.Diagnostics) {
	client, target, ok := r.connect(ctx, plan, diags)
	if !ok {
		return
	}

	name := plan.Name.ValueString()
	desired := permissionsFromSet(ctx, plan.Permissions, diags)
	if diags.HasError() {
		return
	}

	if err := client.EnsureRole(ctx, name); err != nil {
		diags.AddError("Unable to create database role", err.Error())
		return
	}
	if err := client.SyncPermissions(ctx, name, desired); err != nil {
		diags.AddError("Unable to reconcile role permissions", err.Error())
		return
	}
	if err := r.readInto(ctx, client, target, name, &plan); err != nil {
		diags.AddError("Unable to read back database role", err.Error())
		return
	}

	diags.Append(state.Set(ctx, &plan)...)
}

func (r *RoleResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state roleResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, target, ok := r.connect(ctx, state, &resp.Diagnostics)
	if !ok {
		return
	}

	name := state.Name.ValueString()
	exists, err := client.RoleExists(ctx, name)
	if err != nil {
		resp.Diagnostics.AddError("Unable to read database role", err.Error())
		return
	}
	if !exists {
		resp.State.RemoveResource(ctx)
		return
	}

	if err := r.readInto(ctx, client, target, name, &state); err != nil {
		resp.Diagnostics.AddError("Unable to read database role permissions", err.Error())
		return
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *RoleResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state roleResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	client, _, ok := r.connect(ctx, state, &resp.Diagnostics)
	if !ok {
		return
	}

	if err := client.DropRole(ctx, state.Name.ValueString()); err != nil {
		resp.Diagnostics.AddError("Unable to delete database role", err.Error())
	}
}

// ImportState accepts "<server>/<database>/<name>", matching mssql_user.
func (r *RoleResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	parts := strings.SplitN(req.ID, "/", 3)
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		resp.Diagnostics.AddError(
			"Unexpected Import Identifier",
			`Expected an import ID in the form "<server>/<database>/<name>", e.g. "myserver.database.windows.net/mydatabase/db_executor".`,
		)
		return
	}
	server, database, name := parts[0], parts[1], parts[2]

	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("server"), server)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("database"), database)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("name"), name)...)
}

// readInto populates model's id/permissions from current database state
// for the role name at target.
func (r *RoleResource) readInto(ctx context.Context, client *sqlclient.Client, target sqlclient.Target, name string, model *roleResourceModel) error {
	perms, err := client.CurrentPermissions(ctx, name)
	if err != nil {
		return err
	}

	set, diags := permissionsToSet(perms)
	if diags.HasError() {
		return fmt.Errorf("converting permissions for role %q: %v", name, diags)
	}

	model.ID = types.StringValue(fmt.Sprintf("%s/%s/%s", target.Server, target.Database, name))
	model.Name = types.StringValue(name)
	model.Permissions = set
	return nil
}

// permissionsFromSet converts mssql_role.permissions into sqlclient
// permissions, appending any conversion diagnostics to diags.
func permissionsFromSet(ctx context.Context, set types.Set, diags *diag.Diagnostics) []sqlclient.Permission {
	var elements []rolePermissionModel
	diags.Append(set.ElementsAs(ctx, &elements, false)...)

	perms := make([]sqlclient.Permission, 0, len(elements))
	for _, e := range elements {
		perms = append(perms, sqlclient.Permission{
			Permission: e.Permission.ValueString(),
			Class:      e.Class.ValueString(),
			Securable:  e.Securable.ValueString(),
		})
	}
	return perms
}

// permissionsToSet converts sqlclient permissions into the
// mssql_role.permissions set. An empty securable (DATABASE class) becomes
// null so it matches config that omits securable.
func permissionsToSet(perms []sqlclient.Permission) (types.Set, diag.Diagnostics) {
	elements := make([]rolePermissionModel, 0, len(perms))
	for _, p := range perms {
		securable := types.StringNull()
		if p.Securable != "" {
			securable = types.StringValue(p.Securable)
		}
		elements = append(elements, rolePermissionModel{
			Permission: types.StringValue(p.Permission),
			Class:      types.StringValue(p.Class),
			Securable:  securable,
		})
	}
	return types.SetValueFrom(context.Background(), rolePermissionObjectType, elements)
}
