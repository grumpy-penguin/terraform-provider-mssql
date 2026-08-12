terraform {
  required_providers {
    mssql = {
      source = "local/mssql"
    }
  }
}

provider "mssql" {
  # Values below can also be supplied via ARM_TENANT_ID, ARM_CLIENT_ID,
  # ARM_CLIENT_SECRET, MSSQL_SERVER, MSSQL_DATABASE environment
  # variables, which is the recommended way to avoid secrets in state/config.
  tenant_id     = var.tenant_id
  client_id     = var.client_id
  client_secret = var.client_secret

  # server/database here are optional defaults for any mssql_user
  # resource that doesn't set its own — fine for a single fixed server.
  # Omit these and set server/database per-resource instead when the
  # target is created dynamically (e.g. by azurerm in the same apply) —
  # see examples/pipeline-oidc.
  server   = "myserver.database.windows.net"
  database = "mydatabase"
}

variable "tenant_id" {
  type = string
}

variable "client_id" {
  type = string
}

variable "client_secret" {
  type      = string
  sensitive = true
}
