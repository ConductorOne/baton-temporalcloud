package connector

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	identityv1 "go.temporal.io/cloud-sdk/api/identity/v1"
)

var testAccountID = "a1b23"

func TestAccountAccessRoleFromID(t *testing.T) {
	t.Parallel()
	tt := []struct {
		Name     string
		Input    string
		Expected identityv1.AccountAccess_Role
	}{
		{
			Name:     "legacy admin role ID",
			Input:    fmt.Sprintf("example.com-%s", testAccountID),
			Expected: identityv1.AccountAccess_ROLE_ADMIN,
		},
		{
			Name:     "owner",
			Input:    fmt.Sprintf("%s-owner", testAccountID),
			Expected: identityv1.AccountAccess_ROLE_OWNER,
		},
		{
			Name:     "admin",
			Input:    fmt.Sprintf("%s-admin", testAccountID),
			Expected: identityv1.AccountAccess_ROLE_ADMIN,
		},
		{
			Name:     "developer",
			Input:    fmt.Sprintf("%s-developer", testAccountID),
			Expected: identityv1.AccountAccess_ROLE_DEVELOPER,
		},
		{
			Name:     "finance admin",
			Input:    fmt.Sprintf("%s-finance-admin", testAccountID),
			Expected: identityv1.AccountAccess_ROLE_FINANCE_ADMIN,
		},
		{
			Name:     "read-only",
			Input:    fmt.Sprintf("%s-read", testAccountID),
			Expected: identityv1.AccountAccess_ROLE_READ,
		},
		{
			Name:     "invalid",
			Input:    fmt.Sprintf("%s-something-else", testAccountID),
			Expected: identityv1.AccountAccess_ROLE_UNSPECIFIED,
		},
	}

	for _, tc := range tt {
		t.Run(tc.Name, func(t *testing.T) {
			t.Parallel()
			var actual identityv1.AccountAccess_Role
			require.NotPanics(t, func() {
				actual = AccountAccessRoleFromID(tc.Input, testAccountID)
			})
			assert.Equal(t, tc.Expected, actual)
		})
	}
}

func TestNextLowerNamespacePermission(t *testing.T) {
	t.Parallel()
	tt := []struct {
		Name     string
		Input    identityv1.NamespaceAccess_Permission
		Expected identityv1.NamespaceAccess_Permission
	}{
		{
			Name:     "admin downgrades to write",
			Input:    identityv1.NamespaceAccess_PERMISSION_ADMIN,
			Expected: identityv1.NamespaceAccess_PERMISSION_WRITE,
		},
		{
			Name:     "write downgrades to read",
			Input:    identityv1.NamespaceAccess_PERMISSION_WRITE,
			Expected: identityv1.NamespaceAccess_PERMISSION_READ,
		},
		{
			Name:     "read downgrades to unspecified (remove access)",
			Input:    identityv1.NamespaceAccess_PERMISSION_READ,
			Expected: identityv1.NamespaceAccess_PERMISSION_UNSPECIFIED,
		},
		{
			Name:     "unspecified stays unspecified",
			Input:    identityv1.NamespaceAccess_PERMISSION_UNSPECIFIED,
			Expected: identityv1.NamespaceAccess_PERMISSION_UNSPECIFIED,
		},
	}

	for _, tc := range tt {
		t.Run(tc.Name, func(t *testing.T) {
			t.Parallel()
			var actual identityv1.NamespaceAccess_Permission
			require.NotPanics(t, func() {
				actual = nextLowerNamespacePermission(tc.Input)
			})
			assert.Equal(t, tc.Expected, actual)
		})
	}
}

func TestNamespaceAccessPermissionFromString(t *testing.T) {
	t.Parallel()
	tt := []struct {
		Name     string
		Input    string
		Expected identityv1.NamespaceAccess_Permission
	}{
		{
			Name:     "admin permission",
			Input:    "admin",
			Expected: identityv1.NamespaceAccess_PERMISSION_ADMIN,
		},
		{
			Name:     "write permission",
			Input:    "write",
			Expected: identityv1.NamespaceAccess_PERMISSION_WRITE,
		},
		{
			Name:     "read permission",
			Input:    "read",
			Expected: identityv1.NamespaceAccess_PERMISSION_READ,
		},
		{
			Name:     "uppercase ADMIN",
			Input:    "ADMIN",
			Expected: identityv1.NamespaceAccess_PERMISSION_ADMIN,
		},
		{
			Name:     "mixed case Admin",
			Input:    "Admin",
			Expected: identityv1.NamespaceAccess_PERMISSION_ADMIN,
		},
		{
			Name:     "invalid permission returns unspecified",
			Input:    "invalid",
			Expected: identityv1.NamespaceAccess_PERMISSION_UNSPECIFIED,
		},
		{
			Name:     "empty string returns unspecified",
			Input:    "",
			Expected: identityv1.NamespaceAccess_PERMISSION_UNSPECIFIED,
		},
	}

	for _, tc := range tt {
		t.Run(tc.Name, func(t *testing.T) {
			t.Parallel()
			var actual identityv1.NamespaceAccess_Permission
			require.NotPanics(t, func() {
				actual = namespaceAccessPermissionFromString(tc.Input)
			})
			assert.Equal(t, tc.Expected, actual)
		})
	}
}

func TestAccountAccessRoleFromString(t *testing.T) {
	t.Parallel()
	tt := []struct {
		Input     string
		Expected  identityv1.AccountAccess_Role
		ShouldErr bool
	}{
		{
			Input:    "unspecified",
			Expected: identityv1.AccountAccess_ROLE_UNSPECIFIED,
		},
		{
			Input:    "role_unspecified",
			Expected: identityv1.AccountAccess_ROLE_UNSPECIFIED,
		},
		{
			Input:    "owner",
			Expected: identityv1.AccountAccess_ROLE_OWNER,
		},
		{
			Input:    "role_owner",
			Expected: identityv1.AccountAccess_ROLE_OWNER,
		},
		{
			Input:    "admin",
			Expected: identityv1.AccountAccess_ROLE_ADMIN,
		},
		{
			Input:    "role_admin",
			Expected: identityv1.AccountAccess_ROLE_ADMIN,
		},
		{
			Input:    "finance-admin",
			Expected: identityv1.AccountAccess_ROLE_FINANCE_ADMIN,
		},
		{
			Input:    "finance_admin",
			Expected: identityv1.AccountAccess_ROLE_FINANCE_ADMIN,
		},
		{
			Input:    "role_finance_admin",
			Expected: identityv1.AccountAccess_ROLE_FINANCE_ADMIN,
		},
		{
			Input:    "read",
			Expected: identityv1.AccountAccess_ROLE_READ,
		},
		{
			Input:    "role_read",
			Expected: identityv1.AccountAccess_ROLE_READ,
		},
		{
			Input:     "invalid",
			ShouldErr: true,
		},
	}
	for _, tc := range tt {
		t.Run(tc.Input, func(t *testing.T) {
			t.Parallel()
			var actual *identityv1.AccountAccess_Role
			var err error
			require.NotPanics(t, func() {
				actual, err = AccountAccessRoleFromString(tc.Input)
			})
			if tc.ShouldErr {
				assert.Error(t, err)
			} else {
				require.NoError(t, err)
				require.NotNil(t, actual)
				assert.Equal(t, tc.Expected, *actual)
				actual2, err2 := AccountAccessRoleFromString(strings.ToUpper(tc.Input))
				require.NoError(t, err2)
				require.NotNil(t, actual2)
				assert.Equal(t, tc.Expected, *actual2)
			}
		})
	}
}
