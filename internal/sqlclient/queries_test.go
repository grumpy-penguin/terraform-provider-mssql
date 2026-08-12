package sqlclient

import (
	"reflect"
	"testing"
)

func TestDiffRoles(t *testing.T) {
	cases := []struct {
		name              string
		current, desired  []string
		wantAdd, wantDrop []string
	}{
		{
			name:    "no change",
			current: []string{"db_datareader", "db_datawriter"},
			desired: []string{"db_datareader", "db_datawriter"},
		},
		{
			name:    "add only",
			current: []string{"db_datareader"},
			desired: []string{"db_datareader", "db_datawriter"},
			wantAdd: []string{"db_datawriter"},
		},
		{
			name:     "remove only",
			current:  []string{"db_datareader", "db_datawriter"},
			desired:  []string{"db_datareader"},
			wantDrop: []string{"db_datawriter"},
		},
		{
			name:     "add and remove",
			current:  []string{"db_datareader"},
			desired:  []string{"db_datawriter", "custom_role"},
			wantAdd:  []string{"custom_role", "db_datawriter"},
			wantDrop: []string{"db_datareader"},
		},
		{
			name:    "first apply, nothing current",
			current: nil,
			desired: []string{"db_datareader"},
			wantAdd: []string{"db_datareader"},
		},
		{
			name:     "roles list emptied",
			current:  []string{"db_datareader"},
			desired:  nil,
			wantDrop: []string{"db_datareader"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotAdd, gotDrop := diffRoles(tc.current, tc.desired)
			if !reflect.DeepEqual(gotAdd, tc.wantAdd) {
				t.Errorf("toAdd = %v, want %v", gotAdd, tc.wantAdd)
			}
			if !reflect.DeepEqual(gotDrop, tc.wantDrop) {
				t.Errorf("toRemove = %v, want %v", gotDrop, tc.wantDrop)
			}
		})
	}
}

// re-running diffRoles with desired == current (as Read would compute after
// a successful Sync) must always be a no-op — this is the idempotency
// property the whole resource depends on.
func TestDiffRolesIdempotent(t *testing.T) {
	desired := []string{"db_datareader", "db_datawriter", "custom_role"}
	add, drop := diffRoles(desired, desired)
	if len(add) != 0 || len(drop) != 0 {
		t.Fatalf("expected no-op diff when current == desired, got add=%v drop=%v", add, drop)
	}
}
