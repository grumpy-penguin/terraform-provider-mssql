# Stage 2 of 2: workload apply — creates the Azure SQL server (with the
# bootstrapped pipeline identity as its Azure AD Administrator, set AT
# SERVER CREATION), the database, and maps an Azure AD user into it.
#
# tenant_id/client_id come in as variables (populate them from
# bootstrap/'s outputs — e.g. `terraform_remote_state`, CI variables, or
# just pasted once since they rarely change). The mssql provider block
# carries no server/database at all: instead, each mssql_user resource
# sets its own server/database, referencing the azurerm_mssql_server /
# azurerm_mssql_database THIS SAME apply is creating. That's the pattern
# that lets mssql manage users on a server that doesn't exist yet at
# the start of `terraform apply` — a resource can depend on another
# resource's computed output; a provider block can't, reliably.

terraform {
  required_providers {
    azurerm = {
      source = "hashicorp/azurerm"
    }
    mssql = {
      source = "local/mssql"
    }
  }
}

provider "azurerm" {
  features {}
}

# From bootstrap/'s outputs.
variable "tenant_id" {
  type = string
}

variable "client_id" {
  type = string
}

variable "principal_object_id" {
  type = string
}

variable "principal_display_name" {
  type = string
}

provider "mssql" {
  tenant_id = var.tenant_id
  client_id = var.client_id
  use_oidc  = true
  # oidc_request_url / oidc_request_token are left unset here: in a real
  # pipeline they come from ARM_OIDC_REQUEST_URL / ARM_OIDC_REQUEST_TOKEN
  # env vars, or on GitHub Actions are picked up automatically. See
  # "Pipeline authentication (OIDC)" in the main README.
}

resource "azurerm_mssql_server" "this" {
  name                = "myserver"
  resource_group_name = "my-resource-group"
  location            = "eastus"
  version             = "12.0"

  # This is the step that grants CREATE USER / ALTER ROLE rights to the
  # identity mssql connects as — set here, at server creation, not
  # added later.
  azuread_administrator {
    login_username              = var.principal_display_name
    object_id                   = var.principal_object_id
    azuread_authentication_only = true
  }
}

resource "azurerm_mssql_database" "this" {
  name      = "mydatabase"
  server_id = azurerm_mssql_server.this.id
}

resource "mssql_user" "analyst" {
  server    = azurerm_mssql_server.this.fully_qualified_domain_name
  database  = azurerm_mssql_database.this.name
  user_name = "jane.doe@contoso.com"
  roles     = ["db_datareader"]
}
