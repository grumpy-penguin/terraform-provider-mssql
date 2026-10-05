package sqlclient

import (
	"reflect"
	"testing"
)

func TestValidatePermission(t *testing.T) {
	cases := []struct {
		name string
		p    Permission
		err  bool
	}{
		{name: "schema execute", p: Permission{Permission: "EXECUTE", Class: "SCHEMA", Securable: "dbo"}},
		{name: "multi-word permission", p: Permission{Permission: "VIEW DEFINITION", Class: "SCHEMA", Securable: "hr"}},
		{name: "database-level", p: Permission{Permission: "EXECUTE", Class: "DATABASE"}},
		{name: "object", p: Permission{Permission: "EXECUTE", Class: "OBJECT", Securable: "dbo.apisp_Foo"}},
		{name: "type", p: Permission{Permission: "EXECUTE", Class: "TYPE", Securable: "dbo.PhoneEntryIdTVP"}},
		{name: "lower-case permission", p: Permission{Permission: "execute", Class: "SCHEMA", Securable: "dbo"}, err: true},
		{name: "injection attempt in permission", p: Permission{Permission: "EXECUTE TO [x]; DROP TABLE y; --", Class: "DATABASE"}, err: true},
		{name: "unknown class", p: Permission{Permission: "EXECUTE", Class: "ASSEMBLY", Securable: "x"}, err: true},
		{name: "lower-case class", p: Permission{Permission: "EXECUTE", Class: "schema", Securable: "dbo"}, err: true},
		{name: "database with securable", p: Permission{Permission: "EXECUTE", Class: "DATABASE", Securable: "dbo"}, err: true},
		{name: "schema without securable", p: Permission{Permission: "EXECUTE", Class: "SCHEMA"}, err: true},
		{name: "unqualified type", p: Permission{Permission: "EXECUTE", Class: "TYPE", Securable: "PhoneEntryIdTVP"}, err: true},
		{name: "object with empty name part", p: Permission{Permission: "EXECUTE", Class: "OBJECT", Securable: "dbo."}, err: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidatePermission(tc.p)
			if tc.err && err == nil {
				t.Fatalf("expected an error for %+v", tc.p)
			}
			if !tc.err && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestValidateRoleName(t *testing.T) {
	if err := ValidateRoleName("db_executor"); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	for _, name := range []string{"", "public", "PUBLIC"} {
		if err := ValidateRoleName(name); err == nil {
			t.Errorf("expected an error for role name %q", name)
		}
	}
}

func TestPermissionStatement(t *testing.T) {
	cases := []struct {
		name  string
		grant bool
		p     Permission
		role  string
		want  string
	}{
		{
			name:  "grant on schema",
			grant: true,
			p:     Permission{Permission: "EXECUTE", Class: "SCHEMA", Securable: "dbo"},
			role:  "db_executor",
			want:  "GRANT EXECUTE ON SCHEMA::[dbo] TO [db_executor]",
		},
		{
			name:  "grant on type quotes both name parts",
			grant: true,
			p:     Permission{Permission: "EXECUTE", Class: "TYPE", Securable: "dbo.PhoneEntryIdTVP"},
			role:  "db_executor",
			want:  "GRANT EXECUTE ON TYPE::[dbo].[PhoneEntryIdTVP] TO [db_executor]",
		},
		{
			name:  "grant on database has no ON clause",
			grant: true,
			p:     Permission{Permission: "VIEW DEFINITION", Class: "DATABASE"},
			role:  "db_executor",
			want:  "GRANT VIEW DEFINITION TO [db_executor]",
		},
		{
			name:  "revoke cascades",
			grant: false,
			p:     Permission{Permission: "EXECUTE", Class: "OBJECT", Securable: "hr.sp_Leave_Get"},
			role:  "db_executor",
			want:  "REVOKE EXECUTE ON OBJECT::[hr].[sp_Leave_Get] FROM [db_executor] CASCADE",
		},
		{
			name:  "identifiers containing brackets are escaped",
			grant: true,
			p:     Permission{Permission: "EXECUTE", Class: "SCHEMA", Securable: "we]ird"},
			role:  "ro]le",
			want:  "GRANT EXECUTE ON SCHEMA::[we]]ird] TO [ro]]le]",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := permissionStatement(tc.grant, tc.p, tc.role)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("permissionStatement() = %q, want %q", got, tc.want)
			}
		})
	}

	if _, err := permissionStatement(true, Permission{Permission: "EXECUTE; --", Class: "DATABASE"}, "r"); err == nil {
		t.Error("expected permissionStatement to reject an invalid permission")
	}
}

func TestDiffPermissions(t *testing.T) {
	dbo := Permission{Permission: "EXECUTE", Class: "SCHEMA", Securable: "dbo"}
	hr := Permission{Permission: "EXECUTE", Class: "SCHEMA", Securable: "hr"}
	tvp := Permission{Permission: "EXECUTE", Class: "TYPE", Securable: "dbo.PhoneEntryIdTVP"}

	cases := []struct {
		name                 string
		current, desired     []Permission
		wantGrant, wantRevok []Permission
	}{
		{name: "no change", current: []Permission{dbo, hr}, desired: []Permission{hr, dbo}},
		{name: "first apply", desired: []Permission{dbo, tvp}, wantGrant: []Permission{dbo, tvp}},
		{name: "grant and revoke", current: []Permission{dbo, hr}, desired: []Permission{dbo, tvp}, wantGrant: []Permission{tvp}, wantRevok: []Permission{hr}},
		{name: "permissions emptied", current: []Permission{dbo}, wantRevok: []Permission{dbo}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotGrant, gotRevoke := diffPermissions(tc.current, tc.desired)
			if !reflect.DeepEqual(gotGrant, tc.wantGrant) {
				t.Errorf("toGrant = %v, want %v", gotGrant, tc.wantGrant)
			}
			if !reflect.DeepEqual(gotRevoke, tc.wantRevok) {
				t.Errorf("toRevoke = %v, want %v", gotRevoke, tc.wantRevok)
			}
		})
	}
}
