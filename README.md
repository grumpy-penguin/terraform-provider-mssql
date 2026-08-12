# terraform-provider-mssql

A Terraform provider that maps Azure AD (Entra ID) identities to Azure SQL
Database users, and manages which database roles each mapped user belongs
to.

The provider connects to the database as an Azure AD App Registration —
no SQL auth username/password is ever used. Two authentication modes are
supported:

- **Client secret** — a plain App Registration secret. Suitable for local
  development.
- **OIDC workload identity federation** (`use_oidc = true`) — the
  recommended mode when Terraform runs inside a CI/CD pipeline (GitHub
  Actions, Azure DevOps, GitLab CI, ...). The pipeline's own short-lived
  OIDC token is exchanged for an Azure AD access token; no secret is ever
  generated, stored, or rotated. See [Pipeline authentication
  (OIDC)](#pipeline-authentication-oidc) below.

## What it does

Given an Azure AD user or group and a list of database roles, the
`mssql_user` resource:

1. Creates a contained database user mapped to that AAD identity
   (`CREATE USER [name] FROM EXTERNAL PROVIDER`), if one doesn't already
   exist.
2. Reconciles the user's database role memberships to exactly match the
   `roles` list — adding missing memberships (`ALTER ROLE ... ADD MEMBER`)
   and revoking ones no longer listed (`ALTER ROLE ... DROP MEMBER`).

Roles themselves (built-in like `db_datareader`, or custom roles) are
expected to already exist in the database; this resource maps users into
them but does not create or own roles.

### Idempotency

- `Create` checks for an existing principal before issuing `CREATE USER`,
  and treats a duplicate-object error as success (safe under concurrent
  applies).
- `Read` always re-derives state from `sys.database_principals` /
  `sys.database_role_members`, so drift (e.g. someone manually revoking a
  role) shows up as a plan diff instead of being silently ignored.
- `Update` diffs current vs. desired roles and only issues the SQL needed
  to close the gap — re-running `apply` with unchanged config is a no-op.
- Deleting the resource drops the database user (`DROP USER IF EXISTS`),
  which also clears its role memberships.

## Targeting a server and database

`server` and `database` can be set on the **provider** (a default used by
any `mssql_user` that doesn't specify its own) and/or on each
**resource** (overriding that default). Both are optional at the provider
level, but at least one source — provider default or resource-level — must
supply each for a given resource.

Set them on the provider when there's one fixed target for the whole
configuration. Set them per-resource instead when the target varies —
most notably when the Azure SQL server/database is itself created in the
*same* `apply`, by `azurerm` or similar:

```hcl
resource "azurerm_mssql_server" "this" { ... }
resource "azurerm_mssql_database" "this" { ... }

resource "mssql_user" "analyst" {
  server    = azurerm_mssql_server.this.fully_qualified_domain_name
  database  = azurerm_mssql_database.this.name
  user_name = "jane.doe@contoso.com"
  roles     = ["db_datareader"]
}
```

This only works at the resource level, not the provider level — a
`provider` block generally can't be safely configured from a resource's
computed output, because Terraform needs to configure every provider
before it can build the graph of what depends on what, and a provider
depending on that same graph's output is fragile-to-unsupported for a
same-apply create. A `resource` referencing another resource's output is
exactly the normal, well-supported case. Practically: connections are
opened lazily, per resource operation (not once when the provider is
configured), specifically so this works — see
[examples/pipeline-oidc/workload](examples/pipeline-oidc/workload) for
the full pattern.

Connections are cached and reused per distinct server/database, so
multiple `mssql_user` resources pointed at the same target share one
connection pool rather than each opening their own.

## Requirements

- Go 1.25+ (only needed to build the provider itself)
- Terraform >= 1.0
- An Azure AD App Registration that is the **Azure AD Administrator** of
  the target Azure SQL logical server. This is not optional: a database
  principal only gets permission to run `CREATE USER ... FROM EXTERNAL
  PROVIDER` and `ALTER ROLE` if it's the server's AAD admin (or has been
  separately granted equivalent rights by that admin). See
  [Prerequisite: the connecting identity must be the server's Azure AD
  Administrator](#prerequisite-the-connecting-identity-must-be-the-servers-azure-ad-administrator).

## Prerequisite: the connecting identity must be the server's Azure AD Administrator

Azure SQL Database ties the right to create and manage Azure AD-mapped
database users to the logical server's **Azure AD Administrator**. A
regular AAD-mapped user has no such rights by default. So whichever
identity the `mssql` provider connects as — the OIDC-federated pipeline identity, or
a client-secret App Registration — must be set as that server's AAD
admin.

**Set this when the server is created**, not as an afterthought — a
server briefly provisioned without one is a server nothing (including
this provider) can configure AAD users on until you circle back and fix
it. With `azurerm_mssql_server`, that means including the
`azuread_administrator` block in the same `apply` that creates the
server:

```hcl
resource "azuread_application" "pipeline" {
  display_name = "terraform-mssql-pipeline"
}

resource "azuread_service_principal" "pipeline" {
  client_id = azuread_application.pipeline.client_id
}

resource "azurerm_mssql_server" "this" {
  name                = "myserver"
  resource_group_name = azurerm_resource_group.this.name
  location            = azurerm_resource_group.this.location
  version             = "12.0"

  azuread_administrator {
    login_username              = azuread_service_principal.pipeline.display_name
    object_id                   = azuread_service_principal.pipeline.object_id
    azuread_authentication_only = true # optional: disable SQL auth entirely
  }
}
```

If the server already exists without an AAD admin, add the same block (or
use `az sql server ad-admin create` / the portal) before running
`mssql_user` against it — there's no way around setting it somewhere.

You can point the admin at a *group* containing the pipeline identity
instead of the identity directly, which avoids re-provisioning the server
if the pipeline identity ever changes.

The snippet above inlines `azuread_application`/`azuread_service_principal`
for brevity; in practice, provision those separately from the server (see
[examples/pipeline-oidc/bootstrap](examples/pipeline-oidc/bootstrap)) and
pass in their IDs as variables — the same reasoning as
[Targeting a server and database](#targeting-a-server-and-database) above
applies to any resource a *provider block* depends on, not just
`mssql`'s. The full two-stage worked example is in
[examples/pipeline-oidc](examples/pipeline-oidc).

## Building

```bash
go build -o bin/terraform-provider-mssql .
```

## Using it locally (before this is published anywhere)

Point Terraform at the locally built binary with a
[dev_overrides](https://developer.hashicorp.com/terraform/cli/config/config-file#development-overrides-for-provider-developers)
block — no registry or `terraform init` required:

```hcl
# ~/.terraformrc (or any file referenced by TF_CLI_CONFIG_FILE)
provider_installation {
  dev_overrides {
    "local/mssql" = "/absolute/path/to/terraform-provider-mssql/bin"
  }
  direct {}
}
```

```bash
export TF_CLI_CONFIG_FILE=~/.terraformrc
terraform plan
```

## Example

Both authentication modes are first-class; pick whichever matches where
Terraform is running. See
[examples/provider](examples/provider) (client secret),
[examples/provider-oidc](examples/provider-oidc) (OIDC),
[examples/resources/mssql_user](examples/resources/mssql_user), and
[examples/pipeline-oidc](examples/pipeline-oidc) for a full, two-stage
OIDC + AAD-admin-at-creation + dynamic-target walkthrough
([bootstrap](examples/pipeline-oidc/bootstrap) provisions the pipeline
identity once; [workload](examples/pipeline-oidc/workload) is the apply
that runs per environment).

The `mssql_user` resource itself is identical either way — only the
`provider` block differs.

**Client secret** (local development):

```hcl
provider "mssql" {
  # Or leave unset and use ARM_TENANT_ID / ARM_CLIENT_ID / ARM_CLIENT_SECRET
  # environment variables instead, to avoid putting secrets in config.
  tenant_id     = var.tenant_id
  client_id     = var.client_id
  client_secret = var.client_secret
}
```

**OIDC** (CI/CD pipeline — no secret at all):

```hcl
provider "mssql" {
  # tenant_id / client_id can also come from ARM_TENANT_ID / ARM_CLIENT_ID.
  tenant_id = var.tenant_id
  client_id = var.client_id

  use_oidc = true
  # Token source is usually left to environment variables set by the CI
  # system (ARM_OIDC_REQUEST_URL / ARM_OIDC_REQUEST_TOKEN, or on GitHub
  # Actions, picked up automatically) rather than hardcoded here — see
  # "Pipeline authentication (OIDC)" below.
}
```

Either way, `server`/`database` are set on the resource (or, for a single
fixed target, as provider-level defaults — see
[Targeting a server and database](#targeting-a-server-and-database)):

```hcl
resource "mssql_user" "analyst" {
  server    = "myserver.database.windows.net"
  database  = "mydatabase"
  user_name = "jane.doe@contoso.com"
  roles     = ["db_datareader", "reporting_role"]
}
```

Import an existing mapping — the ID is `<server>/<database>/<user_name>`,
since the same provider can manage users across multiple servers:

```bash
terraform import mssql_user.analyst "myserver.database.windows.net/mydatabase/jane.doe@contoso.com"
```

## Pipeline authentication (OIDC)

When Terraform runs in a CI/CD pipeline, prefer `use_oidc = true` over a
client secret: the pipeline's platform-issued OIDC token is exchanged for
an Azure AD access token via a **federated credential** configured on the
App Registration, so no secret is ever created, stored as a pipeline
variable, or rotated.

Setup, once per App Registration:

1. Create (or reuse) the App Registration, and make it the target SQL
   server's Azure AD Administrator — see the prerequisite section above.
2. Add a federated credential on the App Registration trusting your CI
   system's OIDC issuer, scoped to the specific repo/branch or environment
   that should be able to authenticate as it (see
   [examples/pipeline-oidc/bootstrap](examples/pipeline-oidc/bootstrap) for
   the `azuread_application_federated_identity_credential` resource).
3. Configure the provider with `use_oidc = true`, `tenant_id`, and
   `client_id` — no `client_secret`.

**GitHub Actions** — the runner exposes `ACTIONS_ID_TOKEN_REQUEST_URL` /
`ACTIONS_ID_TOKEN_REQUEST_TOKEN` automatically once you request the
`id-token` permission, and this provider reads them as a fallback with no
extra wiring needed:

```yaml
permissions:
  id-token: write
  contents: read

jobs:
  terraform:
    runs-on: ubuntu-latest
    env:
      ARM_USE_OIDC: "true"
      ARM_TENANT_ID: ${{ vars.ARM_TENANT_ID }}
      ARM_CLIENT_ID: ${{ vars.ARM_CLIENT_ID }}
    steps:
      - uses: actions/checkout@v4
      - uses: hashicorp/setup-terraform@v3
      - run: terraform apply -auto-approve
```

**Azure DevOps / other CI systems** — set `ARM_USE_OIDC=true`,
`ARM_TENANT_ID`, `ARM_CLIENT_ID`, and whichever token source your
pipeline provides:

- `ARM_OIDC_TOKEN` — a literal token, if your pipeline step already
  fetched one.
- `ARM_OIDC_TOKEN_FILE_PATH` — a file path, re-read on every token
  refresh (e.g. workload identity federation service connections that
  write a token file).
- `ARM_OIDC_REQUEST_URL` + `ARM_OIDC_REQUEST_TOKEN` — a token endpoint to
  fetch fresh on every refresh, for CI systems that follow the GitHub
  Actions pattern.

## Provider configuration reference

| Argument               | Env var fallback                                                   | Description                                                                  |
| ----------------------- | -------------------------------------------------------------------- | ------------------------------------------------------------------------------ |
| `tenant_id`             | `ARM_TENANT_ID`                                                      | Azure AD tenant ID of the App Registration.                                    |
| `client_id`             | `ARM_CLIENT_ID`                                                      | App Registration (client) ID.                                                  |
| `client_secret`         | `ARM_CLIENT_SECRET`                                                  | App Registration client secret. Sensitive. Unused when `use_oidc` is true.     |
| `use_oidc`              | `ARM_USE_OIDC`                                                       | Authenticate via OIDC workload identity federation instead of a client secret. |
| `oidc_token`            | `ARM_OIDC_TOKEN`                                                     | A literal OIDC JWT. Sensitive.                                                 |
| `oidc_token_file_path`  | `ARM_OIDC_TOKEN_FILE_PATH`                                           | Path to a file containing the OIDC JWT, re-read on every refresh.              |
| `oidc_request_url`      | `ARM_OIDC_REQUEST_URL`, then `ACTIONS_ID_TOKEN_REQUEST_URL`          | CI token endpoint, fetched fresh on every refresh.                             |
| `oidc_request_token`    | `ARM_OIDC_REQUEST_TOKEN`, then `ACTIONS_ID_TOKEN_REQUEST_TOKEN`      | Bearer token authenticating the call to `oidc_request_url`. Sensitive.         |
| `server`                | `MSSQL_SERVER`                                                     | Default Azure SQL logical server FQDN for resources that don't set their own. Optional — see [Targeting a server and database](#targeting-a-server-and-database). |
| `port`                  | —                                                                     | Default SQL port, defaults to `1433`.                                          |
| `database`              | `MSSQL_DATABASE`                                                   | Default target database for resources that don't set their own. Optional.     |

## Resource: `mssql_user`

| Argument    | Required                              | Force replace | Description                                                                                   |
| ----------- | -------------------------------------- | -------------- | ----------------------------------------------------------------------------------------------- |
| `user_name` | yes                                    | yes            | AAD UPN (user) or display name (group) to map.                                                  |
| `server`    | if not set on the provider             | yes            | Azure SQL logical server FQDN. Overrides the provider's `server`.                               |
| `database`  | if not set on the provider             | yes            | Target database. Overrides the provider's `database`.                                           |
| `roles`     | yes                                    | no             | Set of database role names this user should belong to.                                          |

## Releasing

Terraform never builds a provider from source — `terraform init` only
ever downloads a pre-built, versioned, checksummed binary, either from a
registry implementing the [Terraform Registry
Protocol](https://developer.hashicorp.com/terraform/internals/provider-registry-protocol)
or from a local plugin directory/mirror. So getting this onto the public
registry (so `source = "grumpy-penguin/mssql"` works for anyone) means
publishing a release there, which itself requires GPG-signed release
artifacts in a specific layout.

[`.goreleaser.yml`](.goreleaser.yml) and
[`.github/workflows/release.yml`](.github/workflows/release.yml) automate
that: pushing a `vX.Y.Z` tag builds binaries for every OS/arch Terraform
supports, zips them, generates a `SHA256SUMS` file, GPG-signs it, and
attaches everything — plus
[`terraform-registry-manifest.json`](terraform-registry-manifest.json),
which declares the protocol version (6) this provider speaks — to a
GitHub Release.

One-time setup before the first tag (not something this repo can do for
itself):

1. Generate a GPG key (`gpg --full-generate-key`) dedicated to signing
   releases, and add its private key and passphrase to the repo as the
   `GPG_PRIVATE_KEY` / `PASSPHRASE` Actions secrets (Settings → Secrets
   and variables → Actions).
2. Sign in to the [Terraform
   Registry](https://registry.terraform.io/sign-in) with the GitHub
   account that owns this repo, add the same GPG public key under your
   registry account's GPG keys, then use "Publish → Provider" to connect
   this repo. The registry listens for new GitHub Releases matching
   `vX.Y.Z` and picks them up automatically from then on.

After that, releasing is just:

```bash
git tag v0.1.0
git push origin v0.1.0
```

## Status

Initial iteration. Scoped to Azure SQL Database, authenticating via an
Azure AD App Registration (client secret or OIDC workload identity
federation). Release pipeline is in place; not yet published to the
registry — see [Releasing](#releasing).

## Development

```bash
go build ./...
go vet ./...
go test ./...
```

Unit tests cover the pure logic (identifier quoting, SQL error
classification, role-membership diffing, OIDC token handling,
provider/resource target-resolution precedence). Exercising CRUD against
a real Azure SQL Database requires live credentials and is not part of
the test suite.
