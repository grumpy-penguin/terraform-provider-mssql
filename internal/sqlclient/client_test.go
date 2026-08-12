package sqlclient

import (
	"errors"
	"fmt"
	"testing"

	mssql "github.com/microsoft/go-mssqldb"
)

func TestQuoteIdentifier(t *testing.T) {
	cases := map[string]string{
		"jane.doe@contoso.com": "[jane.doe@contoso.com]",
		"db_datareader":        "[db_datareader]",
		"weird]name":           "[weird]]name]",
		"]]leading":            "[]]]]leading]",
	}
	for in, want := range cases {
		if got := quoteIdentifier(in); got != want {
			t.Errorf("quoteIdentifier(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestErrorNumber(t *testing.T) {
	t.Run("direct mssql.Error", func(t *testing.T) {
		err := mssql.Error{Number: 2714, Message: "object already exists"}
		n, ok := errorNumber(err)
		if !ok || n != 2714 {
			t.Fatalf("errorNumber() = (%d, %v), want (2714, true)", n, ok)
		}
	})

	t.Run("wrapped mssql.Error", func(t *testing.T) {
		inner := mssql.Error{Number: 15151, Message: "role membership"}
		wrapped := fmt.Errorf("adding member: %w", inner)
		n, ok := errorNumber(wrapped)
		if !ok || n != 15151 {
			t.Fatalf("errorNumber() = (%d, %v), want (15151, true)", n, ok)
		}
	})

	t.Run("unrelated error", func(t *testing.T) {
		if _, ok := errorNumber(errors.New("boom")); ok {
			t.Fatal("errorNumber() = ok=true for a non-mssql error")
		}
	})
}

func TestIsUniqueViolation(t *testing.T) {
	if !IsUniqueViolation(mssql.Error{Number: 2714}) {
		t.Error("expected error 2714 to be a unique violation")
	}
	if IsUniqueViolation(mssql.Error{Number: 4060}) {
		t.Error("did not expect error 4060 (cannot open database) to be a unique violation")
	}
	if IsUniqueViolation(errors.New("boom")) {
		t.Error("did not expect a non-mssql error to be a unique violation")
	}
}

func TestIsRoleMembershipError(t *testing.T) {
	if !isRoleMembershipError(mssql.Error{Number: 15151}) {
		t.Error("expected error 15151 to be treated as a role membership no-op")
	}
	if isRoleMembershipError(mssql.Error{Number: 18456}) {
		t.Error("did not expect login-failed error 18456 to be treated as a role membership no-op")
	}
}
