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

var accountRoleResourceType = &v2.ResourceType{
	Id:          "account-role",
	DisplayName: "Account Role",
	Traits:      []v2.ResourceType_Trait{v2.ResourceType_TRAIT_ROLE},
}
