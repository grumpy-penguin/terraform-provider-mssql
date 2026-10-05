package provider

import (
	"context"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/grumpy-penguin/terraform-provider-mssql/internal/sqlclient"
)

// stringSetToSlice converts a types.Set of strings into a []string,
// appending any conversion diagnostics to diags.
func stringSetToSlice(ctx context.Context, set types.Set, diags *diag.Diagnostics) []string {
	var out []string
	diags.Append(set.ElementsAs(ctx, &out, false)...)
	return out
}

// stringSliceToSet converts a []string into a types.Set of strings. The
// conversion cannot fail for a plain []string of strings, so diagnostics
// are discarded.
func stringSliceToSet(values []string) types.Set {
	set, _ := types.SetValueFrom(context.Background(), types.StringType, values)
	return set
}

// resolveTarget picks the server/database a resource instance connects
// to: its own server/database if set, otherwise the provider's defaults.
// resourceType only shapes the error message. Returns an error if neither
// source supplies a server or a database.
func resolveTarget(resourceType, server, database, defaultServer string, defaultPort int, defaultDatabase string) (sqlclient.Target, error) {
	if server == "" {
		server = defaultServer
	}
	if server == "" {
		return sqlclient.Target{}, fmt.Errorf("no server configured: set server on the %s resource or on the mssql provider", resourceType)
	}

	if database == "" {
		database = defaultDatabase
	}
	if database == "" {
		return sqlclient.Target{}, fmt.Errorf("no database configured: set database on the %s resource or on the mssql provider", resourceType)
	}

	return sqlclient.Target{Server: server, Port: defaultPort, Database: database}, nil
}
