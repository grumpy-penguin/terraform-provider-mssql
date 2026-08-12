package provider

import (
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/grumpy-penguin/terraform-provider-mssql/internal/sqlclient"
)

func TestResolveTarget(t *testing.T) {
	cases := []struct {
		name string
		r    UserResource
		m    userResourceModel
		want sqlclient.Target
		err  bool
	}{
		{
			name: "resource-level server/database used as-is",
			r:    UserResource{},
			m: userResourceModel{
				Server:   types.StringValue("resource-server.database.windows.net"),
				Database: types.StringValue("resource-db"),
			},
			want: sqlclient.Target{Server: "resource-server.database.windows.net", Database: "resource-db"},
		},
		{
			name: "falls back to provider defaults when resource unset",
			r: UserResource{
				defaultServer:   "provider-server.database.windows.net",
				defaultDatabase: "provider-db",
				defaultPort:     1433,
			},
			m:    userResourceModel{Server: types.StringNull(), Database: types.StringNull()},
			want: sqlclient.Target{Server: "provider-server.database.windows.net", Database: "provider-db", Port: 1433},
		},
		{
			name: "resource-level server overrides provider default, database still falls back",
			r: UserResource{
				defaultServer:   "provider-server.database.windows.net",
				defaultDatabase: "provider-db",
			},
			m: userResourceModel{
				Server:   types.StringValue("override-server.database.windows.net"),
				Database: types.StringNull(),
			},
			want: sqlclient.Target{Server: "override-server.database.windows.net", Database: "provider-db"},
		},
		{
			name: "no server anywhere is an error",
			r:    UserResource{defaultDatabase: "provider-db"},
			m:    userResourceModel{Server: types.StringNull(), Database: types.StringNull()},
			err:  true,
		},
		{
			name: "no database anywhere is an error",
			r:    UserResource{defaultServer: "provider-server.database.windows.net"},
			m:    userResourceModel{Server: types.StringNull(), Database: types.StringNull()},
			err:  true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.r.resolveTarget(tc.m)
			if tc.err {
				if err == nil {
					t.Fatalf("expected an error, got target %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("resolveTarget() = %+v, want %+v", got, tc.want)
			}
		})
	}
}
