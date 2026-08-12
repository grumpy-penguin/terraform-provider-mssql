package provider

import (
	"context"
	"os"
	"strconv"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/grumpy-penguin/terraform-provider-mssql/internal/sqlclient"
)

// Ensure MssqlProvider satisfies the expected interfaces.
var _ provider.Provider = &MssqlProvider{}

// MssqlProvider maps Azure AD identities to Azure SQL Database users and
// their database role memberships, authenticating as an Azure AD App
// Registration (service principal).
type MssqlProvider struct {
	version string
}

// mssqlProviderModel is the provider configuration schema.
type mssqlProviderModel struct {
	TenantID     types.String `tfsdk:"tenant_id"`
	ClientID     types.String `tfsdk:"client_id"`
	ClientSecret types.String `tfsdk:"client_secret"`

	UseOIDC          types.Bool   `tfsdk:"use_oidc"`
	OIDCToken        types.String `tfsdk:"oidc_token"`
	OIDCTokenFile    types.String `tfsdk:"oidc_token_file_path"`
	OIDCRequestURL   types.String `tfsdk:"oidc_request_url"`
	OIDCRequestToken types.String `tfsdk:"oidc_request_token"`

	Server   types.String `tfsdk:"server"`
	Port     types.Int64  `tfsdk:"port"`
	Database types.String `tfsdk:"database"`
}

// providerData is what's handed to each resource's Configure via
// resp.ResourceData: the resolved credential (shared by every target), a
// client cache keyed by target so resources pointed at the same
// server/database reuse one connection pool, and the provider-level
// server/port/database to fall back to when a resource doesn't specify
// its own — letting a single mssql provider manage users across
// multiple servers (e.g. ones created dynamically by other providers in
// the same apply) without every resource repeating the connection
// details.
type providerData struct {
	cache *clientCache

	defaultServer   string
	defaultPort     int
	defaultDatabase string
}

func New(version string) func() provider.Provider {
	return func() provider.Provider {
		return &MssqlProvider{version: version}
	}
}

func (p *MssqlProvider) Metadata(_ context.Context, _ provider.MetadataRequest, resp *provider.MetadataResponse) {
	resp.TypeName = "mssql"
	resp.Version = p.version
}

func (p *MssqlProvider) Schema(_ context.Context, _ provider.SchemaRequest, resp *provider.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Maps Azure AD (Entra ID) identities to Azure SQL Database users and manages their database role memberships. Connects to the database as an Azure AD App Registration, either via a client secret or, for CI/CD pipelines, via OIDC workload identity federation (use_oidc). " +
			"Whichever identity is used must be the SQL Server's Azure AD Administrator (set at server creation, or added afterwards) — that's what grants it permission to run CREATE USER ... FROM EXTERNAL PROVIDER and manage role membership; see the provider README for setup guidance.",
		Attributes: map[string]schema.Attribute{
			"tenant_id": schema.StringAttribute{
				Optional:    true,
				Description: "Azure AD tenant ID of the App Registration. Falls back to the ARM_TENANT_ID environment variable.",
			},
			"client_id": schema.StringAttribute{
				Optional:    true,
				Description: "Client (application) ID of the Azure AD App Registration used to authenticate to the database. Falls back to the ARM_CLIENT_ID environment variable.",
			},
			"client_secret": schema.StringAttribute{
				Optional:    true,
				Sensitive:   true,
				Description: "Client secret of the Azure AD App Registration. Falls back to the ARM_CLIENT_SECRET environment variable. Not used, and not required, when use_oidc is true.",
			},
			"use_oidc": schema.BoolAttribute{
				Optional: true,
				Description: "Authenticate via OIDC workload identity federation instead of a client secret — the pattern for CI/CD pipelines (GitHub Actions, Azure DevOps, GitLab CI) that issue their own short-lived OIDC tokens. " +
					"Requires a federated credential configured on the client_id App Registration trusting the pipeline's OIDC issuer, and one of oidc_token, oidc_token_file_path, or oidc_request_url/oidc_request_token to supply the pipeline's token. Falls back to the ARM_USE_OIDC environment variable.",
			},
			"oidc_token": schema.StringAttribute{
				Optional:    true,
				Sensitive:   true,
				Description: "A pre-fetched OIDC JWT to present as the client assertion. Falls back to the ARM_OIDC_TOKEN environment variable. Only used when use_oidc is true.",
			},
			"oidc_token_file_path": schema.StringAttribute{
				Optional:    true,
				Description: "Path to a file containing the OIDC JWT, re-read on every token refresh (e.g. a Kubernetes projected service account token, or an Azure DevOps workload identity federation token file). Falls back to the ARM_OIDC_TOKEN_FILE_PATH environment variable. Only used when use_oidc is true.",
			},
			"oidc_request_url": schema.StringAttribute{
				Optional: true,
				Description: "URL of the CI system's OIDC token endpoint, fetched fresh on every token refresh (the GitHub Actions pattern). " +
					"Falls back to the ARM_OIDC_REQUEST_URL environment variable, then to GitHub Actions' own ACTIONS_ID_TOKEN_REQUEST_URL. Only used when use_oidc is true.",
			},
			"oidc_request_token": schema.StringAttribute{
				Optional:  true,
				Sensitive: true,
				Description: "Bearer token used to authenticate the request to oidc_request_url; this authenticates the call to the CI system's token endpoint and is distinct from the OIDC token it returns. " +
					"Falls back to the ARM_OIDC_REQUEST_TOKEN environment variable, then to GitHub Actions' own ACTIONS_ID_TOKEN_REQUEST_TOKEN. Only used when use_oidc is true.",
			},
			"server": schema.StringAttribute{
				Optional: true,
				Description: "Default Azure SQL logical server name, e.g. myserver.database.windows.net, used by any mssql_user resource that doesn't set its own server. Falls back to the MSSQL_SERVER environment variable. " +
					"Optional here: leave unset (and set server per-resource instead) when the target server is created in the same apply by another provider, since a provider block generally can't reference another resource's not-yet-known attributes the way a resource block can.",
			},
			"port": schema.Int64Attribute{
				Optional:    true,
				Description: "Default TCP port of the SQL server, used when a resource doesn't set its own. Defaults to 1433.",
			},
			"database": schema.StringAttribute{
				Optional:    true,
				Description: "Default target database, used by any mssql_user resource that doesn't set its own database. Falls back to the MSSQL_DATABASE environment variable.",
			},
		},
	}
}

func (p *MssqlProvider) Configure(ctx context.Context, req provider.ConfigureRequest, resp *provider.ConfigureResponse) {
	var model mssqlProviderModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &model)...)
	if resp.Diagnostics.HasError() {
		return
	}

	auth := sqlclient.AuthConfig{
		TenantID:     firstNonEmpty(model.TenantID.ValueString(), os.Getenv("ARM_TENANT_ID")),
		ClientID:     firstNonEmpty(model.ClientID.ValueString(), os.Getenv("ARM_CLIENT_ID")),
		ClientSecret: firstNonEmpty(model.ClientSecret.ValueString(), os.Getenv("ARM_CLIENT_SECRET")),

		UseOIDC:           firstNonEmptyBool(model.UseOIDC, os.Getenv("ARM_USE_OIDC")),
		OIDCToken:         firstNonEmpty(model.OIDCToken.ValueString(), os.Getenv("ARM_OIDC_TOKEN")),
		OIDCTokenFilePath: firstNonEmpty(model.OIDCTokenFile.ValueString(), os.Getenv("ARM_OIDC_TOKEN_FILE_PATH")),
		OIDCRequestURL:    firstNonEmpty(model.OIDCRequestURL.ValueString(), os.Getenv("ARM_OIDC_REQUEST_URL"), os.Getenv("ACTIONS_ID_TOKEN_REQUEST_URL")),
		OIDCRequestToken:  firstNonEmpty(model.OIDCRequestToken.ValueString(), os.Getenv("ARM_OIDC_REQUEST_TOKEN"), os.Getenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN")),
	}

	data := providerData{
		defaultServer:   firstNonEmpty(model.Server.ValueString(), os.Getenv("MSSQL_SERVER")),
		defaultDatabase: firstNonEmpty(model.Database.ValueString(), os.Getenv("MSSQL_DATABASE")),
	}
	if !model.Port.IsNull() {
		data.defaultPort = int(model.Port.ValueInt64())
	}

	if auth.TenantID == "" {
		resp.Diagnostics.AddAttributeError(path.Root("tenant_id"), "Missing tenant_id", "Set tenant_id in the provider block or the ARM_TENANT_ID environment variable.")
	}
	if auth.ClientID == "" {
		resp.Diagnostics.AddAttributeError(path.Root("client_id"), "Missing client_id", "Set client_id in the provider block or the ARM_CLIENT_ID environment variable.")
	}

	if auth.UseOIDC {
		if auth.OIDCToken == "" && auth.OIDCTokenFilePath == "" && auth.OIDCRequestURL == "" {
			resp.Diagnostics.AddAttributeError(path.Root("use_oidc"), "Missing OIDC token source",
				"use_oidc is true but none of oidc_token, oidc_token_file_path, or oidc_request_url (and ARM_OIDC_* / ACTIONS_ID_TOKEN_REQUEST_* equivalents) were set.")
		}
		if auth.ClientSecret != "" {
			resp.Diagnostics.AddAttributeWarning(path.Root("client_secret"), "client_secret is ignored",
				"use_oidc is true, so client_secret is not used for authentication.")
		}
	} else if auth.ClientSecret == "" {
		resp.Diagnostics.AddAttributeError(path.Root("client_secret"), "Missing client_secret",
			"Set client_secret in the provider block or the ARM_CLIENT_SECRET environment variable, or set use_oidc = true to authenticate via OIDC workload identity federation instead.")
	}

	if resp.Diagnostics.HasError() {
		return
	}

	// Identity is resolved here, once, but no connection is opened yet:
	// resources may point at a server/database created later in the same
	// apply (e.g. by azurerm), which won't be known until that resource is
	// actually applied. Each mssql_user resource connects lazily, on its
	// own Create/Read/Update/Delete, via the shared cache below.
	credential, err := sqlclient.NewCredential(auth)
	if err != nil {
		resp.Diagnostics.AddError("Unable to build Azure AD credential", err.Error())
		return
	}
	data.cache = newClientCache(credential)

	resp.DataSourceData = &data
	resp.ResourceData = &data
}

func (p *MssqlProvider) Resources(_ context.Context) []func() resource.Resource {
	return []func() resource.Resource{
		NewUserResource,
	}
}

func (p *MssqlProvider) DataSources(_ context.Context) []func() datasource.DataSource {
	return nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// firstNonEmptyBool returns configured if it was explicitly set in HCL,
// otherwise parses envFallback (e.g. ARM_USE_OIDC="true"), defaulting to
// false if neither is set or envFallback doesn't parse as a bool.
func firstNonEmptyBool(configured types.Bool, envFallback string) bool {
	if !configured.IsNull() {
		return configured.ValueBool()
	}
	b, _ := strconv.ParseBool(envFallback)
	return b
}
