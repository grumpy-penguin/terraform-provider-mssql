package sqlclient

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
)

// PrincipalType mirrors the relevant subset of sys.database_principals.type.
type PrincipalType string

const (
	PrincipalTypeExternalUser  PrincipalType = "E" // AAD user/service principal
	PrincipalTypeExternalGroup PrincipalType = "X" // AAD group
	PrincipalTypeRole          PrincipalType = "R"
	PrincipalTypeUnknown       PrincipalType = ""
)

// PrincipalInfo describes an existing database principal.
type PrincipalInfo struct {
	Exists bool
	Type   PrincipalType
}

// GetPrincipal looks up a database principal (user, group, or role) by name.
func (c *Client) GetPrincipal(ctx context.Context, name string) (PrincipalInfo, error) {
	row := c.db.QueryRowContext(ctx,
		`SELECT type FROM sys.database_principals WHERE name = @p1`, sql.Named("p1", name))

	var t string
	switch err := row.Scan(&t); err {
	case nil:
		return PrincipalInfo{Exists: true, Type: PrincipalType(t)}, nil
	case sql.ErrNoRows:
		return PrincipalInfo{Exists: false}, nil
	default:
		return PrincipalInfo{}, fmt.Errorf("looking up principal %q: %w", name, err)
	}
}

// EnsureExternalUser creates a database user mapped to the given Azure AD
// identity (FROM EXTERNAL PROVIDER) if it does not already exist. It is
// idempotent: if the user already exists as an external user or group, it is
// left untouched. If it exists as a different principal type (e.g. a native
// SQL user), an error is returned rather than silently overwriting it.
func (c *Client) EnsureExternalUser(ctx context.Context, userName string) error {
	info, err := c.GetPrincipal(ctx, userName)
	if err != nil {
		return err
	}
	if info.Exists {
		if info.Type != PrincipalTypeExternalUser && info.Type != PrincipalTypeExternalGroup {
			return fmt.Errorf("principal %q already exists in the database but is not an Azure AD user/group (type=%q); refusing to modify it", userName, info.Type)
		}
		return nil // already mapped, nothing to do
	}

	stmt := fmt.Sprintf(`CREATE USER %s FROM EXTERNAL PROVIDER`, quoteIdentifier(userName))
	if _, err := c.db.ExecContext(ctx, stmt); err != nil {
		if IsUniqueViolation(err) {
			return nil // created concurrently by another apply; treat as success
		}
		return fmt.Errorf("creating external user %q: %w", userName, err)
	}
	return nil
}

// DropUser removes a database user if it exists. SQL Server automatically
// clears the user's role memberships as part of DROP USER.
func (c *Client) DropUser(ctx context.Context, userName string) error {
	stmt := fmt.Sprintf(`DROP USER IF EXISTS %s`, quoteIdentifier(userName))
	if _, err := c.db.ExecContext(ctx, stmt); err != nil {
		return fmt.Errorf("dropping user %q: %w", userName, err)
	}
	return nil
}

// CurrentRoles returns the sorted list of database role names the given
// user is currently a member of.
func (c *Client) CurrentRoles(ctx context.Context, userName string) ([]string, error) {
	rows, err := c.db.QueryContext(ctx, `
		SELECT r.name
		FROM sys.database_role_members rm
		JOIN sys.database_principals r ON rm.role_principal_id = r.principal_id
		JOIN sys.database_principals u ON rm.member_principal_id = u.principal_id
		WHERE u.name = @p1
		ORDER BY r.name`, sql.Named("p1", userName))
	if err != nil {
		return nil, fmt.Errorf("listing roles for %q: %w", userName, err)
	}
	defer rows.Close()

	var roles []string
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			return nil, fmt.Errorf("scanning role row: %w", err)
		}
		roles = append(roles, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating roles for %q: %w", userName, err)
	}
	sort.Strings(roles)
	return roles, nil
}

// RoleExists reports whether a database role with the given name exists.
// Roles are expected to be managed outside this resource (e.g. pre-created
// built-in roles like db_datareader, or custom roles managed elsewhere);
// this provider maps users into roles but does not create/own roles.
func (c *Client) RoleExists(ctx context.Context, roleName string) (bool, error) {
	info, err := c.GetPrincipal(ctx, roleName)
	if err != nil {
		return false, err
	}
	return info.Exists && info.Type == PrincipalTypeRole, nil
}

// AddRoleMember adds userName as a member of roleName. Idempotent: if the
// user is already a member, this is a no-op.
func (c *Client) AddRoleMember(ctx context.Context, roleName, userName string) error {
	exists, err := c.RoleExists(ctx, roleName)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("database role %q does not exist; create it before assigning members", roleName)
	}

	stmt := fmt.Sprintf(`ALTER ROLE %s ADD MEMBER %s`, quoteIdentifier(roleName), quoteIdentifier(userName))
	if _, err := c.db.ExecContext(ctx, stmt); err != nil {
		if isRoleMembershipError(err) {
			return nil
		}
		return fmt.Errorf("adding %q to role %q: %w", userName, roleName, err)
	}
	return nil
}

// DropRoleMember removes userName from roleName. Idempotent: if the user is
// not a member, this is a no-op.
func (c *Client) DropRoleMember(ctx context.Context, roleName, userName string) error {
	stmt := fmt.Sprintf(`ALTER ROLE %s DROP MEMBER %s`, quoteIdentifier(roleName), quoteIdentifier(userName))
	if _, err := c.db.ExecContext(ctx, stmt); err != nil {
		if isRoleMembershipError(err) {
			return nil
		}
		return fmt.Errorf("removing %q from role %q: %w", userName, roleName, err)
	}
	return nil
}

// SyncRoles reconciles the user's current role memberships to exactly match
// desired, adding and removing memberships as needed. Returns the roles
// actually added/removed for logging/diagnostics.
func (c *Client) SyncRoles(ctx context.Context, userName string, desired []string) (added, removed []string, err error) {
	current, err := c.CurrentRoles(ctx, userName)
	if err != nil {
		return nil, nil, err
	}

	toAdd, toRemove := diffRoles(current, desired)

	for _, role := range toAdd {
		if err := c.AddRoleMember(ctx, role, userName); err != nil {
			return added, removed, err
		}
		added = append(added, role)
	}
	for _, role := range toRemove {
		if err := c.DropRoleMember(ctx, role, userName); err != nil {
			return added, removed, err
		}
		removed = append(removed, role)
	}

	sort.Strings(added)
	sort.Strings(removed)
	return added, removed, nil
}

// diffRoles compares a user's current role memberships against the desired
// set and returns which roles need to be added and which need to be
// removed to reconcile the two. Pure and side-effect free so it can be
// exercised directly in tests without a database connection.
func diffRoles(current, desired []string) (toAdd, toRemove []string) {
	currentSet := toSet(current)
	desiredSet := toSet(desired)

	for _, role := range desired {
		if !currentSet[role] {
			toAdd = append(toAdd, role)
		}
	}
	for _, role := range current {
		if !desiredSet[role] {
			toRemove = append(toRemove, role)
		}
	}

	sort.Strings(toAdd)
	sort.Strings(toRemove)
	return toAdd, toRemove
}

func toSet(items []string) map[string]bool {
	set := make(map[string]bool, len(items))
	for _, i := range items {
		set[i] = true
	}
	return set
}
