# Creates a custom database role and grants it EXECUTE on the schemas an
# application's stored procedures live in. No fixed database role grants
# EXECUTE, so a stored-procedure-only identity needs a role like this.
# Re-applying is idempotent: only the GRANT/REVOKE statements needed to
# reconcile the role's actual permissions with this set are issued.
resource "mssql_role" "executor" {
  server   = "myserver.database.windows.net"
  database = "mydatabase"
  name     = "db_executor"

  permissions = [
    { permission = "EXECUTE", class = "SCHEMA", securable = "dbo" },
    { permission = "EXECUTE", class = "SCHEMA", securable = "hr" },
    # Table-valued parameter types need their own EXECUTE grant.
    { permission = "EXECUTE", class = "TYPE", securable = "dbo.PhoneEntryIdTVP" },
  ]
}

# Members are added through mssql_user. Referencing mssql_role.executor.name
# makes Terraform create the role before the membership.
resource "mssql_user" "api" {
  server    = "myserver.database.windows.net"
  database  = "mydatabase"
  user_name = "my-api-managed-identity"

  roles = [
    mssql_role.executor.name,
  ]
}
