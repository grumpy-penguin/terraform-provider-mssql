package provider

import (
	"context"
	"reflect"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/grumpy-penguin/terraform-provider-mssql/internal/sqlclient"
)

func TestRoleResolveTargetFallsBackToProviderDefaults(t *testing.T) {
	r := RoleResource{defaultServer: "provider-server.database.windows.net", defaultDatabase: "provider-db"}
	got, err := r.resolveTarget(roleResourceModel{Server: types.StringNull(), Database: types.StringNull()})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := sqlclient.Target{Server: "provider-server.database.windows.net", Database: "provider-db"}
	if got != want {
		t.Errorf("resolveTarget() = %+v, want %+v", got, want)
	}

	if _, err := (&RoleResource{}).resolveTarget(roleResourceModel{Server: types.StringNull(), Database: types.StringNull()}); err == nil {
		t.Error("expected an error when no server/database is configured anywhere")
	}
}

// Read converts what the database reports back into the permissions set;
// that conversion must round-trip exactly, and a DATABASE-class grant's
// empty securable must come back as null (matching config that omits it),
// or every plan would show a diff.
func TestPermissionsSetRoundTrip(t *testing.T) {
	perms := []sqlclient.Permission{
		{Permission: "EXECUTE", Class: "SCHEMA", Securable: "dbo"},
		{Permission: "EXECUTE", Class: "TYPE", Securable: "dbo.PhoneEntryIdTVP"},
		{Permission: "VIEW DEFINITION", Class: "DATABASE"},
	}

	set, diags := permissionsToSet(perms)
	if diags.HasError() {
		t.Fatalf("permissionsToSet: %v", diags)
	}

	var elements []rolePermissionModel
	if d := set.ElementsAs(context.Background(), &elements, false); d.HasError() {
		t.Fatalf("ElementsAs: %v", d)
	}
	for _, e := range elements {
		if e.Class.ValueString() == "DATABASE" && !e.Securable.IsNull() {
			t.Errorf("DATABASE-class securable = %v, want null", e.Securable)
		}
	}

	var d diag.Diagnostics
	back := permissionsFromSet(context.Background(), set, &d)
	if d.HasError() {
		t.Fatalf("permissionsFromSet: %v", d)
	}
	if !reflect.DeepEqual(toPermissionSet(back), toPermissionSet(perms)) {
		t.Errorf("round trip = %v, want %v", back, perms)
	}
}

func toPermissionSet(perms []sqlclient.Permission) map[sqlclient.Permission]bool {
	set := make(map[sqlclient.Permission]bool, len(perms))
	for _, p := range perms {
		set[p] = true
	}
	return set
}
