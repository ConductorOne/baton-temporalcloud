package connector

import (
	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
)

// The user resource type is for all user objects from the database.
var userResourceType = &v2.ResourceType{
	Id:          "user",
	DisplayName: "User",
	Traits:      []v2.ResourceType_Trait{v2.ResourceType_TRAIT_USER},
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
