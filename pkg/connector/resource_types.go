package connector

import (
	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	"github.com/conductorone/baton-sdk/pkg/annotations"

	identityv1 "go.temporal.io/cloud-sdk/api/identity/v1"
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
// resource types C1 vends and the connector revokes. A key is owned by exactly
// one user or one service account, so there is one secret resource type per
// owner kind. That keeps each type's owning identity unambiguous, which is what
// lets an issuer declare a single subject type.
//
// SkipEntitlementsAndGrants is set because an API key carries no permissions of
// its own: it inherits its owner's. The required provider authority is a role
// on the connector's own principal (Global Administrator or Account Owner),
// not a per-key permission, so it is documented in docs/connector.mdx rather
// than advertised through CapabilityPermissions, whose free-form strings this
// provider's role-based model has no honest value for.
var serviceAccountAPIKeyResourceType = &v2.ResourceType{
	Id:          "service-account-api-key",
	DisplayName: "Service Account API Key",
	Description: "A Temporal Cloud API key owned by, and carrying the permissions of, one service account.",
	Traits:      []v2.ResourceType_Trait{v2.ResourceType_TRAIT_SECRET},
	Annotations: annotations.New(&v2.SkipEntitlementsAndGrants{}),
}

var userAPIKeyResourceType = &v2.ResourceType{
	Id:          "user-api-key",
	DisplayName: "User API Key",
	Description: "A Temporal Cloud API key owned by, and carrying the permissions of, one user.",
	Traits:      []v2.ResourceType_Trait{v2.ResourceType_TRAIT_SECRET},
	Annotations: annotations.New(&v2.SkipEntitlementsAndGrants{}),
}

// apiKeyKind ties a secret resource type to the provider owner kind it covers
// and the identity resource type that owner is synced as.
type apiKeyKind struct {
	resourceType      *v2.ResourceType
	ownerType         identityv1.OwnerType
	ownerResourceType *v2.ResourceType
}

var serviceAccountAPIKeyKind = apiKeyKind{
	resourceType:      serviceAccountAPIKeyResourceType,
	ownerType:         identityv1.OwnerType_OWNER_TYPE_SERVICE_ACCOUNT,
	ownerResourceType: serviceAccountResourceType,
}

var userAPIKeyKind = apiKeyKind{
	resourceType:      userAPIKeyResourceType,
	ownerType:         identityv1.OwnerType_OWNER_TYPE_USER,
	ownerResourceType: userResourceType,
}

var accountRoleResourceType = &v2.ResourceType{
	Id:          "account-role",
	DisplayName: "Account Role",
	Traits:      []v2.ResourceType_Trait{v2.ResourceType_TRAIT_ROLE},
}
