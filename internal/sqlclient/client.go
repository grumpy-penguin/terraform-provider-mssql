// Package sqlclient provides a thin, purpose-built client for provisioning
// Azure AD-mapped database users and role memberships in Azure SQL Database.
package sqlclient

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	mssql "github.com/microsoft/go-mssqldb"
	"github.com/microsoft/go-mssqldb/msdsn"
)

// AuthConfig holds the Azure AD identity used to authenticate to Azure SQL
// Database — who connects, as opposed to Target, which says where. Two
// mutually exclusive authentication modes are supported: a client secret,
// or OIDC workload identity federation (UseOIDC) for CI/CD pipelines. See
// buildCredential.
type AuthConfig struct {
	TenantID     string
	ClientID     string
	ClientSecret string

	UseOIDC           bool
	OIDCToken         string // literal JWT
	OIDCTokenFilePath string // path to a file containing the JWT
	OIDCRequestURL    string // CI token endpoint, e.g. $ACTIONS_ID_TOKEN_REQUEST_URL
	OIDCRequestToken  string // bearer token for the above, e.g. $ACTIONS_ID_TOKEN_REQUEST_TOKEN
}

// Target identifies the specific Azure SQL Database to connect to.
// Separated from AuthConfig so a single resolved identity can connect to
// different servers/databases specified per-resource (e.g. one created by
// another provider in the same apply), rather than being pinned to one
// target for the whole provider configuration.
type Target struct {
	Server   string // FQDN, e.g. myserver.database.windows.net
	Port     int    // defaults to 1433
	Database string
}

// normalizedPort returns t.Port, defaulting to 1433 if unset.
func (t Target) normalizedPort() int {
	if t.Port == 0 {
		return 1433
	}
	return t.Port
}

// Key returns a stable string uniquely identifying this target, suitable
// for use as a connection-cache key.
func (t Target) Key() string {
	return fmt.Sprintf("%s:%d/%s", t.Server, t.normalizedPort(), t.Database)
}

// Client wraps a *sql.DB connected to Azure SQL Database via Azure AD
// authentication.
type Client struct {
	db *sql.DB
}

// NewCredential builds the Azure AD credential described by auth, without
// making any network calls or requiring a Target — it authenticates as an
// identity, not yet to anywhere in particular. Resolve one per provider
// Configure and reuse it across every Target-specific Connect call.
func NewCredential(auth AuthConfig) (azcore.TokenCredential, error) {
	return buildCredential(auth)
}

// Connect opens a connection pool to Azure SQL Database at target,
// authenticating with cred (see NewCredential).
func Connect(ctx context.Context, cred azcore.TokenCredential, target Target) (*Client, error) {
	if target.Server == "" || target.Database == "" {
		return nil, fmt.Errorf("server and database are required")
	}

	dsnConfig, err := msdsn.Parse(dsn(target))
	if err != nil {
		return nil, fmt.Errorf("building connection config: %w", err)
	}

	// NewSecurityTokenConnector calls this provider fresh for every new
	// physical connection (including reconnects and pool growth), so token
	// refresh — for both the client-secret and OIDC credential — is handled
	// by azidentity's own caching without any extra work here.
	connector, err := mssql.NewSecurityTokenConnector(dsnConfig, func(ctx context.Context) (string, error) {
		tok, err := cred.GetToken(ctx, policy.TokenRequestOptions{Scopes: []string{databaseScope}})
		if err != nil {
			return "", fmt.Errorf("acquiring Azure AD access token: %w", err)
		}
		return tok.Token, nil
	})
	if err != nil {
		return nil, fmt.Errorf("building connector: %w", err)
	}

	db := sql.OpenDB(connector)
	db.SetMaxOpenConns(5)
	db.SetMaxIdleConns(2)

	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("connecting to %s/%s: %w", target.Server, target.Database, err)
	}

	return &Client{db: db}, nil
}

// dsn builds the non-authentication portion of the connection string;
// authentication is layered on separately via NewSecurityTokenConnector.
func dsn(target Target) string {
	values := map[string]string{
		"server":       target.Server,
		"port":         fmt.Sprintf("%d", target.normalizedPort()),
		"database":     target.Database,
		"encrypt":      "true",
		"app name":     "terraform-provider-mssql",
		"dial timeout": "15",
		"conn timeout": "30",
	}
	var b strings.Builder
	first := true
	for k, v := range values {
		if !first {
			b.WriteString(";")
		}
		first = false
		b.WriteString(k)
		b.WriteString("=")
		b.WriteString(strings.ReplaceAll(v, ";", ""))
	}
	return b.String()
}

// Close closes the underlying connection pool.
func (c *Client) Close() error {
	if c == nil || c.db == nil {
		return nil
	}
	return c.db.Close()
}

// quoteIdentifier safely brackets a SQL Server identifier (mirrors T-SQL's
// QUOTENAME) so principal/role names can be safely interpolated into DDL,
// which cannot be parameterized.
func quoteIdentifier(name string) string {
	return "[" + strings.ReplaceAll(name, "]", "]]") + "]"
}

// errorNumber extracts the SQL Server error number from err, if err (or
// something it wraps) is an mssql.Error.
func errorNumber(err error) (int32, bool) {
	for err != nil {
		if e, ok := err.(mssql.Error); ok {
			return e.Number, true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return 0, false
		}
		err = u.Unwrap()
	}
	return 0, false
}

// IsUniqueViolation reports whether err represents a SQL Server duplicate
// key/object-already-exists error, used to make Create idempotent under
// concurrent applies.
func IsUniqueViolation(err error) bool {
	n, ok := errorNumber(err)
	if !ok {
		return false
	}
	switch n {
	case 2714, 15023: // "There is already an object named..." / user already exists
		return true
	}
	return false
}

// isRoleMembershipError reports whether err is SQL Server error 15151,
// raised both when adding a member that's already in the role and when
// dropping a member that isn't — in both cases the desired end state
// already holds, so callers treat it as a no-op.
func isRoleMembershipError(err error) bool {
	n, ok := errorNumber(err)
	return ok && n == 15151
}
