package connector

import (
	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	"github.com/conductorone/baton-sdk/pkg/annotations"
)

// The user resource type is for all user objects from the database.
var userResourceType = &v2.ResourceType{
	Id:          "user",
	DisplayName: "User",
	Traits:      []v2.ResourceType_Trait{v2.ResourceType_TRAIT_USER},
	Annotations: annotations.New(&v2.SkipEntitlementsAndGrants{}),
}

// Temporal Cloud service accounts are a distinct identity from users (GetUsers
// returns only humans); they are synced separately and emit ACCOUNT_TYPE_SERVICE.
var serviceAccountResourceType = &v2.ResourceType{
	Id:          "service-account",
	DisplayName: "Service Account",
	Traits:      []v2.ResourceType_Trait{v2.ResourceType_TRAIT_USER},
	Annotations: annotations.New(&v2.SkipEntitlementsAndGrants{}),
}

var namespaceResourceType = &v2.ResourceType{
	Id:          "namespace",
	DisplayName: "Namespace",
}

// Temporal Cloud API keys are credentials, not identities: they are the secret
// resource type C1 vends and the connector revokes. The type is declared and
// synced so issuance has a resource type to return and a ResourceDeleterV2 to
// revoke through, both of which the SDK and C1 require before they will offer a
// credential kind.
//
// SkipEntitlementsAndGrants is set because an API key carries no permissions of
// its own: it inherits its owner's. The required provider authority is a role
// on the connector's own principal (Global Administrator or Account Owner),
// not a per-key permission, so it is documented in docs/connector.mdx rather
// than advertised through CapabilityPermissions, whose free-form strings this
// provider's role-based model has no honest value for.
var apiKeyResourceType = &v2.ResourceType{
	Id:          "api-key",
	DisplayName: "API Key",
	Description: "A Temporal Cloud API key. Owned by, and carrying the permissions of, one user or service account.",
	Traits:      []v2.ResourceType_Trait{v2.ResourceType_TRAIT_SECRET},
	Annotations: annotations.New(&v2.SkipEntitlementsAndGrants{}),
}

var accountRoleResourceType = &v2.ResourceType{
	Id:          "account-role",
	DisplayName: "Account Role",
	Traits:      []v2.ResourceType_Trait{v2.ResourceType_TRAIT_ROLE},
}
