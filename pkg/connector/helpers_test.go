package connector

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"

	identityv1 "go.temporal.io/api/cloud/identity/v1"
)

var testAccountID = "a1b23"

func TestAccountAccessRoleFromID(t *testing.T) {
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
		tc := tc
		t.Run(tc.Name, func(t *testing.T) {
			actual := accountAccessRoleFromID(tc.Input, testAccountID)
			assert.Equal(t, tc.Expected, actual)
		})
	}
}
