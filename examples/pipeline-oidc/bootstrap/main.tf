# Stage 1 of 2: identity bootstrap.
#
# Run this ONCE, as its own apply/state, separately from workload/. It
# creates the App Registration the pipeline authenticates as and trusts
# the CI system's OIDC issuer — nothing here touches SQL. Its outputs
# (tenant_id, client_id, principal_object_id) then feed into workload/ as
# variables.
#
# Why separate: a provider block generally can't safely be configured
# from a resource created in the SAME apply (Terraform needs to configure
# every provider before it can build the resource graph, so a provider
# depending on that graph's own output is fragile/unsupported for the
# create case). tenant_id/client_id are provider-level (mssql) config,
# so the identity they refer to needs to already exist by the time
# workload/ runs — hence its own stage. This is exactly the same reason
# server/database live on mssql_user resources instead of the mssql
# provider block: resource-to-resource references work fine, provider
# block-to-resource references don't.

terraform {
  required_providers {
    azuread = {
      source = "hashicorp/azuread"
    }
  }
}

resource "azuread_application" "pipeline" {
  display_name = "terraform-mssql-pipeline"
}

resource "azuread_service_principal" "pipeline" {
  client_id = azuread_application.pipeline.client_id
}

# Trust the CI system's OIDC issuer — no client secret is created or
# stored. Example shown for GitHub Actions; adjust issuer/subject for
# Azure DevOps, GitLab CI, etc. Scope the subject to the exact repo/branch
# (or environment) that should be able to mint tokens as this identity.
resource "azuread_application_federated_identity_credential" "github_actions" {
  application_id = azuread_application.pipeline.id
  display_name   = "github-actions-main"
  audiences      = ["api://AzureADTokenExchange"]
  issuer         = "https://token.actions.githubusercontent.com"
  subject        = "repo:my-org/my-repo:ref:refs/heads/main"
}

output "tenant_id" {
  value = azuread_service_principal.pipeline.application_tenant_id
}

output "client_id" {
  value = azuread_application.pipeline.client_id
}

output "principal_object_id" {
  description = "Feed this into workload/'s azurerm_mssql_server.azuread_administrator.object_id."
  value       = azuread_service_principal.pipeline.object_id
}

output "principal_display_name" {
  value = azuread_service_principal.pipeline.display_name
}
