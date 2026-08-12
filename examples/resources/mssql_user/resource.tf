# Maps an Azure AD user to an Azure SQL Database user and assigns it a set
# of database roles. Re-applying is idempotent: the resource only issues
# the SQL needed to reconcile actual role membership with this list, and a
# plan against unchanged config produces no diff.
#
# server/database here are optional: if the mssql provider block already
# sets its own server/database, every resource that doesn't set its own
# inherits those as defaults. Set them explicitly (as below) to target a
# specific server/database per resource — e.g. one this same apply is
# creating via another provider. See examples/pipeline-oidc for that case.
resource "mssql_user" "analyst" {
  server    = "myserver.database.windows.net"
  database  = "mydatabase"
  user_name = "jane.doe@contoso.com"

  roles = [
    "db_datareader",
    "reporting_role", # a pre-existing custom database role
  ]
}

# Azure AD groups can be mapped the same way; SQL Server treats them
# identically to users for FROM EXTERNAL PROVIDER purposes. This one
# targets a different database on the same server.
resource "mssql_user" "etl_group" {
  server    = "myserver.database.windows.net"
  database  = "otherdatabase"
  user_name = "ETL-Service-Accounts"

  roles = [
    "db_datareader",
    "db_datawriter",
  ]
}
