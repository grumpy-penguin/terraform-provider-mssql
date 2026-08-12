terraform {
  required_providers {
    mssql = {
      source = "local/mssql"
    }
  }
}

# OIDC workload identity federation — the recommended auth mode when
# Terraform runs inside a CI/CD pipeline. No client secret is created,
# stored, or rotated: the pipeline's own short-lived OIDC token is
# exchanged for an Azure AD access token via a federated credential
# configured on the App Registration (see examples/pipeline-oidc for how
# to set that up, including making the App Registration the target SQL
# server's Azure AD Administrator).
provider "mssql" {
  tenant_id = var.tenant_id
  client_id = var.client_id

  use_oidc = true

  # Token source: pick whichever matches your CI system. All three are
  # optional here since each also has an environment variable fallback
  # (ARM_OIDC_TOKEN / ARM_OIDC_TOKEN_FILE_PATH / ARM_OIDC_REQUEST_URL +
  # ARM_OIDC_REQUEST_TOKEN) — in practice most pipelines set the env vars
  # instead of hardcoding a token source in config. On GitHub Actions
  # specifically, oidc_request_url/oidc_request_token don't need to be set
  # at all: they're picked up automatically from the runner's
  # ACTIONS_ID_TOKEN_REQUEST_URL / ACTIONS_ID_TOKEN_REQUEST_TOKEN.
  #
  # oidc_request_url   = "https://token.actions.githubusercontent.com/..."
  # oidc_request_token = var.oidc_request_token

  # Optional defaults for any mssql_user resource that doesn't set its
  # own server/database. Omit these and set server/database per-resource
  # instead when targeting a server created dynamically in the same
  # apply — see examples/pipeline-oidc/workload, which is the more
  # realistic shape for a pipeline that also provisions its own database.
  server   = "myserver.database.windows.net"
  database = "mydatabase"
}

variable "tenant_id" {
  type = string
}

variable "client_id" {
  type = string
}
