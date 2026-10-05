package sqlclient

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Securable classes a Permission can be granted on. Names follow T-SQL's
// own GRANT ... ON <class>:: syntax; DATABASE is the database itself and
// takes no securable name.
const (
	SecurableClassDatabase = "DATABASE"
	SecurableClassSchema   = "SCHEMA"
	SecurableClassObject   = "OBJECT"
	SecurableClassType     = "TYPE"
)

// Permission is a single permission granted to a database role, e.g.
// EXECUTE on SCHEMA::dbo. Securable is empty for SecurableClassDatabase,
// a schema name for SecurableClassSchema, and "<schema>.<name>" for
// SecurableClassObject/SecurableClassType.
type Permission struct {
	Permission string
	Class      string
	Securable  string
}

// permissionNamePattern matches T-SQL permission names (EXECUTE, SELECT,
// VIEW DEFINITION, ALTER ANY SCHEMA, ...). Permission names can't be
// parameterized or bracket-quoted in GRANT/REVOKE, so this is what keeps
// them safe to interpolate into DDL.
var permissionNamePattern = regexp.MustCompile(`^[A-Z]+( [A-Z]+)*$`)

// ValidatePermission checks p is well-formed without touching a database:
// an upper-case permission name, a supported class, and a securable whose
// presence/shape matches that class. Upper case is required (rather than
// normalized) because Read reports permissions back from
// sys.database_permissions in upper case, and anything else would show as
// a perpetual diff.
func ValidatePermission(p Permission) error {
	if !permissionNamePattern.MatchString(p.Permission) {
		return fmt.Errorf("permission %q must be an upper-case T-SQL permission name, e.g. EXECUTE or VIEW DEFINITION", p.Permission)
	}

	switch p.Class {
	case SecurableClassDatabase:
		if p.Securable != "" {
			return fmt.Errorf("securable must not be set when class is %s (got %q)", SecurableClassDatabase, p.Securable)
		}
	case SecurableClassSchema:
		if p.Securable == "" {
			return fmt.Errorf("securable (a schema name) is required when class is %s", SecurableClassSchema)
		}
	case SecurableClassObject, SecurableClassType:
		if _, _, ok := splitQualifiedName(p.Securable); !ok {
			return fmt.Errorf("securable must be \"<schema>.<name>\" when class is %s (got %q)", p.Class, p.Securable)
		}
	default:
		return fmt.Errorf("class %q must be one of %s, %s, %s, %s", p.Class,
			SecurableClassDatabase, SecurableClassSchema, SecurableClassObject, SecurableClassType)
	}
	return nil
}

// ValidateRoleName rejects names this provider must never take ownership
// of. "public" is the implicit role every user belongs to; fixed roles
// (db_owner, db_datareader, ...) are rejected at apply time by EnsureRole,
// since that needs a database lookup.
func ValidateRoleName(name string) error {
	if name == "" {
		return fmt.Errorf("role name must not be empty")
	}
	if strings.EqualFold(name, "public") {
		return fmt.Errorf("the public role can't be managed by mssql_role")
	}
	return nil
}

// splitQualifiedName splits "<schema>.<name>" on its first dot.
func splitQualifiedName(qualified string) (schema, name string, ok bool) {
	schema, name, ok = strings.Cut(qualified, ".")
	return schema, name, ok && schema != "" && name != ""
}

// permissionStatement builds the GRANT or REVOKE statement for p on role.
// REVOKE uses CASCADE so it also succeeds for a permission that was
// granted WITH GRANT OPTION outside Terraform.
func permissionStatement(grant bool, p Permission, role string) (string, error) {
	if err := ValidatePermission(p); err != nil {
		return "", err
	}

	on := ""
	switch p.Class {
	case SecurableClassSchema:
		on = fmt.Sprintf(" ON SCHEMA::%s", quoteIdentifier(p.Securable))
	case SecurableClassObject, SecurableClassType:
		schema, name, _ := splitQualifiedName(p.Securable)
		on = fmt.Sprintf(" ON %s::%s.%s", p.Class, quoteIdentifier(schema), quoteIdentifier(name))
	}

	if grant {
		return fmt.Sprintf("GRANT %s%s TO %s", p.Permission, on, quoteIdentifier(role)), nil
	}
	return fmt.Sprintf("REVOKE %s%s FROM %s CASCADE", p.Permission, on, quoteIdentifier(role)), nil
}

// EnsureRole creates a custom database role if it doesn't already exist.
// An existing custom role of the same name is adopted as-is; anything else
// with that name (a user, or a fixed role like db_datareader) is an error
// rather than something to silently take ownership of.
func (c *Client) EnsureRole(ctx context.Context, name string) error {
	row := c.db.QueryRowContext(ctx,
		`SELECT type, is_fixed_role FROM sys.database_principals WHERE name = @p1`, sql.Named("p1", name))

	var t string
	var fixed bool
	switch err := row.Scan(&t, &fixed); err {
	case nil:
		if PrincipalType(t) != PrincipalTypeRole {
			return fmt.Errorf("principal %q already exists in the database but is not a role (type=%q); refusing to modify it", name, t)
		}
		if fixed {
			return fmt.Errorf("%q is a fixed database role; mssql_role only manages custom roles", name)
		}
		return nil // already exists, adopt it
	case sql.ErrNoRows:
	default:
		return fmt.Errorf("looking up role %q: %w", name, err)
	}

	stmt := fmt.Sprintf(`CREATE ROLE %s`, quoteIdentifier(name))
	if _, err := c.db.ExecContext(ctx, stmt); err != nil {
		if IsUniqueViolation(err) {
			return nil // created concurrently by another apply; treat as success
		}
		return fmt.Errorf("creating role %q: %w", name, err)
	}
	return nil
}

// DropRole removes a custom database role if it exists. SQL Server refuses
// to drop a role that still has members, so they're removed first; the
// role's own permissions go with it automatically.
func (c *Client) DropRole(ctx context.Context, name string) error {
	exists, err := c.RoleExists(ctx, name)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}

	members, err := c.roleMembers(ctx, name)
	if err != nil {
		return err
	}
	for _, member := range members {
		if err := c.DropRoleMember(ctx, name, member); err != nil {
			return err
		}
	}

	stmt := fmt.Sprintf(`DROP ROLE IF EXISTS %s`, quoteIdentifier(name))
	if _, err := c.db.ExecContext(ctx, stmt); err != nil {
		return fmt.Errorf("dropping role %q: %w", name, err)
	}
	return nil
}

// roleMembers returns the names of every principal that is a member of
// roleName.
func (c *Client) roleMembers(ctx context.Context, roleName string) ([]string, error) {
	rows, err := c.db.QueryContext(ctx, `
		SELECT m.name
		FROM sys.database_role_members rm
		JOIN sys.database_principals r ON rm.role_principal_id = r.principal_id
		JOIN sys.database_principals m ON rm.member_principal_id = m.principal_id
		WHERE r.name = @p1`, sql.Named("p1", roleName))
	if err != nil {
		return nil, fmt.Errorf("listing members of role %q: %w", roleName, err)
	}
	defer rows.Close()

	var members []string
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			return nil, fmt.Errorf("scanning role member row: %w", err)
		}
		members = append(members, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating members of role %q: %w", roleName, err)
	}
	return members, nil
}

// CurrentPermissions returns the sorted permissions currently granted to
// roleName on the securable classes this provider manages. DENY entries
// and column-level grants aren't managed here and are excluded, so they
// never show up as drift.
func (c *Client) CurrentPermissions(ctx context.Context, roleName string) ([]Permission, error) {
	rows, err := c.db.QueryContext(ctx, `
		SELECT
			p.permission_name,
			CASE p.class
				WHEN 0 THEN 'DATABASE'
				WHEN 1 THEN 'OBJECT'
				WHEN 3 THEN 'SCHEMA'
				WHEN 6 THEN 'TYPE'
			END,
			CASE p.class
				WHEN 0 THEN ''
				WHEN 1 THEN OBJECT_SCHEMA_NAME(p.major_id) + '.' + OBJECT_NAME(p.major_id)
				WHEN 3 THEN SCHEMA_NAME(p.major_id)
				WHEN 6 THEN (SELECT SCHEMA_NAME(t.schema_id) + '.' + t.name FROM sys.types t WHERE t.user_type_id = p.major_id)
			END
		FROM sys.database_permissions p
		JOIN sys.database_principals r ON p.grantee_principal_id = r.principal_id
		WHERE r.name = @p1
			AND p.state IN ('G', 'W')
			AND p.class IN (0, 1, 3, 6)
			AND p.minor_id = 0`, sql.Named("p1", roleName))
	if err != nil {
		return nil, fmt.Errorf("listing permissions for role %q: %w", roleName, err)
	}
	defer rows.Close()

	var perms []Permission
	for rows.Next() {
		var p Permission
		var securable sql.NullString
		if err := rows.Scan(&p.Permission, &p.Class, &securable); err != nil {
			return nil, fmt.Errorf("scanning permission row: %w", err)
		}
		p.Securable = securable.String
		perms = append(perms, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating permissions for role %q: %w", roleName, err)
	}
	sortPermissions(perms)
	return perms, nil
}

// SyncPermissions reconciles roleName's permissions to exactly match
// desired. Grants are applied before revokes so a change that swaps one
// permission for another never leaves the role with less access than
// either the old or new config intended.
func (c *Client) SyncPermissions(ctx context.Context, roleName string, desired []Permission) error {
	current, err := c.CurrentPermissions(ctx, roleName)
	if err != nil {
		return err
	}

	toGrant, toRevoke := diffPermissions(current, desired)

	for _, p := range toGrant {
		stmt, err := permissionStatement(true, p, roleName)
		if err != nil {
			return err
		}
		if _, err := c.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("granting %s to role %q: %w", describePermission(p), roleName, err)
		}
	}
	for _, p := range toRevoke {
		stmt, err := permissionStatement(false, p, roleName)
		if err != nil {
			return err
		}
		if _, err := c.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("revoking %s from role %q: %w", describePermission(p), roleName, err)
		}
	}
	return nil
}

// diffPermissions compares a role's current permissions against the
// desired set and returns which need granting and which need revoking.
// Pure and side-effect free so it can be exercised directly in tests
// without a database connection.
func diffPermissions(current, desired []Permission) (toGrant, toRevoke []Permission) {
	currentSet := make(map[Permission]bool, len(current))
	for _, p := range current {
		currentSet[p] = true
	}
	desiredSet := make(map[Permission]bool, len(desired))
	for _, p := range desired {
		desiredSet[p] = true
	}

	for _, p := range desired {
		if !currentSet[p] {
			toGrant = append(toGrant, p)
		}
	}
	for _, p := range current {
		if !desiredSet[p] {
			toRevoke = append(toRevoke, p)
		}
	}

	sortPermissions(toGrant)
	sortPermissions(toRevoke)
	return toGrant, toRevoke
}

func sortPermissions(perms []Permission) {
	sort.Slice(perms, func(i, j int) bool {
		a, b := perms[i], perms[j]
		if a.Class != b.Class {
			return a.Class < b.Class
		}
		if a.Securable != b.Securable {
			return a.Securable < b.Securable
		}
		return a.Permission < b.Permission
	})
}

func describePermission(p Permission) string {
	if p.Class == SecurableClassDatabase {
		return p.Permission
	}
	return fmt.Sprintf("%s on %s::%s", p.Permission, p.Class, p.Securable)
}
