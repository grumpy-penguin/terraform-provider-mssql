package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/types"
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
