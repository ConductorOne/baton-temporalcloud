package connector

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"

	"golang.org/x/text/cases"
	"golang.org/x/text/language"
	"google.golang.org/protobuf/proto"

	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	"github.com/conductorone/baton-sdk/pkg/pagination"
	"github.com/conductorone/baton-sdk/pkg/types/entitlement"
	"github.com/conductorone/baton-sdk/pkg/types/grant"
	rs "github.com/conductorone/baton-sdk/pkg/types/resource"
	"github.com/fatih/camelcase"
	"go.uber.org/zap"

	cloudservicev1 "go.temporal.io/cloud-sdk/api/cloudservice/v1"
	identityv1 "go.temporal.io/cloud-sdk/api/identity/v1"
	namespacev1 "go.temporal.io/cloud-sdk/api/namespace/v1"
	operationv1 "go.temporal.io/cloud-sdk/api/operation/v1"
)

// protoUserToResource builds a user resource. Temporal Cloud's GetUsers returns
// only human identities — service accounts are a separate identity fetched via
// GetServiceAccounts (see protoServiceAccountToResource) — so HUMAN is correct here.
func protoUserToResource(proto *identityv1.User) (*v2.Resource, error) {
	annos := &v2.V1Identifier{
		Id: fmt.Sprintf("user:%s", proto.GetSpec().GetEmail()),
	}

	user, err := rs.NewUserResource(proto.GetSpec().GetEmail(), userResourceType, proto.GetId(), []rs.UserTraitOption{
		rs.WithEmail(proto.GetSpec().GetEmail(), true),
		rs.WithAccountType(v2.UserTrait_ACCOUNT_TYPE_HUMAN),
	},
		rs.WithResourceCreatedAt(proto.GetCreatedTime().AsTime()), rs.WithAnnotation(annos))
	if err != nil {
		return nil, err
	}
	return user, nil
}

// protoServiceAccountToResource builds a resource for a Temporal Cloud service
// account, emitting ACCOUNT_TYPE_SERVICE so the identity is classified as
// non-human at ingest (the SDK would otherwise default an unset type to HUMAN).
func protoServiceAccountToResource(proto *identityv1.ServiceAccount) (*v2.Resource, error) {
	name := proto.GetSpec().GetName()
	if name == "" {
		name = proto.GetId()
	}

	sa, err := rs.NewUserResource(name, serviceAccountResourceType, proto.GetId(), []rs.UserTraitOption{
		rs.WithAccountType(v2.UserTrait_ACCOUNT_TYPE_SERVICE),
	},
		rs.WithResourceCreatedAt(proto.GetCreatedTime().AsTime()))
	if err != nil {
		return nil, err
	}
	return sa, nil
}

func protoNamespaceToResource(proto *namespacev1.Namespace) (*v2.Resource, error) {
	annos := &v2.V1Identifier{
		Id: fmt.Sprintf("namespace:%s", proto.GetNamespace()),
	}

	ns, err := rs.NewResource(proto.GetNamespace(), namespaceResourceType, proto.GetNamespace(), rs.WithAnnotation(annos))
	if err != nil {
		return nil, err
	}

	return ns, nil
}

func protoAccountRoleToResource(proto identityv1.AccountAccess_Role, accountID string) (*v2.Resource, error) {
	ar := accountRoleName(proto)
	annos := &v2.V1Identifier{
		Id: fmt.Sprintf("account-role:%s", ar),
	}
	role, err := rs.NewRoleResource(accountRoleDisplayName(proto), accountRoleResourceType, getAccountRoleID(proto, accountID), []rs.RoleTraitOption{}, rs.WithAnnotation(annos))
	if err != nil {
		return nil, err
	}
	return role, nil
}

// protoUserGroupToResource builds a group resource. The group kind is recorded
// in the group profile because it determines whether membership is manageable
// through Temporal Cloud's member APIs (Cloud groups) or owned by an external
// identity provider (SCIM and Google groups).
func protoUserGroupToResource(group *identityv1.UserGroup) (*v2.Resource, error) {
	spec := group.GetSpec()

	profile := map[string]interface{}{}
	switch kind := groupKindFromSpec(spec); kind {
	case groupKindGoogle:
		profile["group_kind"] = kind
		profile["google_email"] = spec.GetGoogleGroup().GetEmailAddress()
	case groupKindScim:
		profile["group_kind"] = kind
		profile["scim_idp_id"] = spec.GetScimGroup().GetIdpId()
	default:
		profile["group_kind"] = kind
	}

	annos := &v2.V1Identifier{
		Id: fmt.Sprintf("group:%s", group.GetId()),
	}

	displayName := spec.GetDisplayName()
	if displayName == "" {
		displayName = group.GetId()
	}

	groupResource, err := rs.NewGroupResource(displayName, groupResourceType, group.GetId(), nil,
		rs.WithResourceCreatedAt(group.GetCreatedTime().AsTime()),
		rs.WithAnnotation(annos),
		rs.WithResourceProfile(profile),
	)
	if err != nil {
		return nil, err
	}
	return groupResource, nil
}

func createNamespaceGrant(user *identityv1.User, namespace *v2.Resource, permission identityv1.NamespaceAccess_Permission) (*v2.Grant, error) {
	perm := namespacePermissionName(permission)
	ur, err := protoUserToResource(user)
	if err != nil {
		return nil, err
	}
	annos := &v2.V1Identifier{
		Id: grantID(namespaceEntitlementID(namespace.GetId().GetResource(), perm), ur.GetId().GetResource()),
	}
	g := grant.NewGrant(namespace, perm, ur.GetId(), grant.WithAnnotation(annos))
	g.Principal = ur
	return g, nil
}

var immutableAccountRoles = []identityv1.AccountAccess_Role{
	identityv1.AccountAccess_ROLE_FINANCE_ADMIN,
	identityv1.AccountAccess_ROLE_OWNER,
}

func createAccountRoleGrant(user *identityv1.User, ar *v2.Resource, accountID string) (*v2.Grant, error) {
	ur, err := protoUserToResource(user)
	if err != nil {
		return nil, err
	}

	annos := []proto.Message{
		&v2.V1Identifier{
			Id: grantID(membershipEntitlementID(ar.GetId().GetResource()), ur.GetId().GetResource()),
		},
	}

	accountRole := AccountAccessRoleFromID(ar.GetId().GetResource(), accountID)
	if slices.Contains(immutableAccountRoles, accountRole) {
		annos = append(annos, &v2.GrantImmutable{})
	}

	g := grant.NewGrant(ar, roleMemberEntitlement, ur.GetId(), grant.WithAnnotation(annos...))
	return g, nil
}

// newGroupAccountRoleGrant builds a grant of an account role to a user group.
// The grant is expandable over the group's member entitlement so group members
// transitively receive the role.
func newGroupAccountRoleGrant(groupResource *v2.Resource, ar *v2.Resource, accountID string) *v2.Grant {
	annos := []proto.Message{
		&v2.V1Identifier{
			Id: grantID(membershipEntitlementID(ar.GetId().GetResource()), groupResource.GetId().GetResource()),
		},
		&v2.GrantExpandable{
			EntitlementIds: []string{entitlement.NewEntitlementID(groupResource, groupMemberEntitlement)},
		},
	}

	accountRole := AccountAccessRoleFromID(ar.GetId().GetResource(), accountID)
	if slices.Contains(immutableAccountRoles, accountRole) {
		annos = append(annos, &v2.GrantImmutable{})
	}

	return grant.NewGrant(ar, roleMemberEntitlement, groupResource.GetId(), grant.WithAnnotation(annos...))
}

// newGroupNamespaceGrant builds a grant of a namespace permission to a user
// group. The grant is expandable over the group's member entitlement so group
// members transitively receive the permission.
func newGroupNamespaceGrant(groupResource *v2.Resource, namespace *v2.Resource, permission identityv1.NamespaceAccess_Permission) *v2.Grant {
	perm := namespacePermissionName(permission)
	annos := []proto.Message{
		&v2.V1Identifier{
			Id: grantID(namespaceEntitlementID(namespace.GetId().GetResource(), perm), groupResource.GetId().GetResource()),
		},
		&v2.GrantExpandable{
			EntitlementIds: []string{entitlement.NewEntitlementID(groupResource, groupMemberEntitlement)},
		},
	}

	return grant.NewGrant(namespace, perm, groupResource.GetId(), grant.WithAnnotation(annos...))
}

func awaitAsyncOperation(ctx context.Context, l *zap.Logger, client cloudservicev1.CloudServiceClient, requestID string, retryDelay time.Duration) error {
	complete, err := checkAsyncOperation(ctx, client, requestID)
	if err != nil {
		return err
	}

	for !complete {
		select {
		case <-ctx.Done():
			return fmt.Errorf("operation timed out: %w", ctx.Err())
		case <-time.After(retryDelay):
		}

		l.Debug("baton-temporalcloud: waiting for operation to complete, checking status...")
		complete, err = checkAsyncOperation(ctx, client, requestID)
		if err != nil {
			return err
		}
	}

	return nil
}

func checkAsyncOperation(ctx context.Context, client cloudservicev1.CloudServiceClient, requestID string) (bool, error) {
	resp, err := client.GetAsyncOperation(ctx, &cloudservicev1.GetAsyncOperationRequest{AsyncOperationId: requestID})
	if err != nil {
		return false, fmt.Errorf("could not check operation status: %w", err)
	}

	op := resp.GetAsyncOperation()

	switch op.State {
	case operationv1.AsyncOperation_STATE_PENDING, operationv1.AsyncOperation_STATE_IN_PROGRESS:
	case operationv1.AsyncOperation_STATE_FAILED:
		return false, fmt.Errorf("operation failed: %s", op.GetFailureReason())
	case operationv1.AsyncOperation_STATE_CANCELLED:
		return false, fmt.Errorf("operation failed: operation was cancelled")
	case operationv1.AsyncOperation_STATE_FULFILLED:
		return true, nil
	default:
	}

	return false, nil
}

func paginate[T any](rv T, bag *pagination.Bag, pageToken string) (T, *rs.SyncOpResults, error) {
	if pageToken == "" {
		return rv, nil, nil
	}

	token, err := bag.NextToken(pageToken)
	if err != nil {
		return rv, nil, err
	}
	return rv, &rs.SyncOpResults{NextPageToken: token}, nil
}

// paginateGrants advances the pagination bag used by multi-phase grants syncs.
// When the current API page is exhausted it moves to the next phase; when no
// phases remain it returns no results to end the sync.
func paginateGrants(rv []*v2.Grant, bag *pagination.Bag, pageToken string) ([]*v2.Grant, *rs.SyncOpResults, error) {
	if pageToken != "" {
		if err := bag.Next(pageToken); err != nil {
			return nil, nil, err
		}
	} else {
		bag.Pop()
	}

	token, err := bag.Marshal()
	if err != nil {
		return nil, nil, err
	}
	if token == "" {
		return rv, nil, nil
	}
	return rv, &rs.SyncOpResults{NextPageToken: token}, nil
}

const (
	membershipEntitlementIDTemplate = "membership:%s"
	namespaceEntitlementIDTemplate  = "namespace:%s:%s"
	grantIDTemplate                 = "grant:%s:%s"
	groupMemberEntitlement          = "member"
)

func grantID(entitlementID string, userID string) string {
	return fmt.Sprintf(grantIDTemplate, entitlementID, userID)
}

func membershipEntitlementID(resourceID string) string {
	return fmt.Sprintf(membershipEntitlementIDTemplate, resourceID)
}

func namespaceEntitlementID(resourceID string, role string) string {
	return fmt.Sprintf(namespaceEntitlementIDTemplate, resourceID, role)
}

const (
	groupKindCloud      = "cloud"
	groupKindGoogle     = "google"
	groupKindScim       = "scim"
	groupKindProfileKey = "group_kind"
)

func groupKindFromSpec(spec *identityv1.UserGroupSpec) string {
	switch {
	case spec.GetCloudGroup() != nil:
		return groupKindCloud
	case spec.GetGoogleGroup() != nil:
		return groupKindGoogle
	case spec.GetScimGroup() != nil:
		return groupKindScim
	}
	return ""
}

func fromStringToEnum(prefix string, in string) string {
	in = strings.Map(func(r rune) rune {
		if r == '-' {
			return '_'
		}
		return unicode.ToUpper(r)
	}, in)
	return fmt.Sprintf("%s_%s", prefix, in)
}

// AccountAccessRoleFromStringOrDefault is like AccountAccessRoleFromString, but will return
// AccountAccess_ROLE_UNSPECIFIED instead of an error.
func AccountAccessRoleFromStringOrDefault(in string) identityv1.AccountAccess_Role {
	role, err := AccountAccessRoleFromString(in)
	if err != nil {
		return identityv1.AccountAccess_ROLE_UNSPECIFIED
	}
	return *role
}

// AccountAccessRoleFromString parses a string into an AccountAccess_Role using the following (case-insensitive) mapping:
//
//	AccountAccess_ROLE_UNSPECIFIED: "unspecified", "role_unspecified"
//	AccountAccess_ROLE_OWNER: "owner", "role_owner"
//	AccountAccess_ROLE_ADMIN: "admin", "role_admin"
//	AccountAccess_ROLE_DEVELOPER: "developer", "role_developer"
//	AccountAccess_ROLE_FINANCE_ADMIN: "finance-admin", "finance_admin", "role_finance_admin"
//	AccountAccess_ROLE_READ: "read", "role_read"
//
// Any unknown values with return an error.
func AccountAccessRoleFromString(in string) (*identityv1.AccountAccess_Role, error) {
	if role, ok := identityv1.AccountAccess_Role_value[strings.ToUpper(in)]; ok {
		rv := identityv1.AccountAccess_Role(role)
		return &rv, nil
	}
	needle := fromStringToEnum("ROLE", in)
	val, ok := identityv1.AccountAccess_Role_value[needle]
	if !ok {
		return nil, fmt.Errorf("unknown AccountAccess_Role: %s", needle)
	}
	rv := identityv1.AccountAccess_Role(val)
	return &rv, nil
}

func namespaceAccessPermissionFromString(in string) identityv1.NamespaceAccess_Permission {
	needle := fromStringToEnum("PERMISSION", in)
	rv, ok := identityv1.NamespaceAccess_Permission_value[needle]
	if !ok {
		return identityv1.NamespaceAccess_PERMISSION_UNSPECIFIED
	}
	return identityv1.NamespaceAccess_Permission(rv)
}

func AccountAccessRoleFromID(in string, accountID string) identityv1.AccountAccess_Role {
	if strings.HasSuffix(in, accountID) { // handle legacy admin role ID
		return identityv1.AccountAccess_ROLE_ADMIN
	}

	role := strings.TrimPrefix(in, accountID+"-")
	return AccountAccessRoleFromStringOrDefault(role)
}

func accountRoleName(in identityv1.AccountAccess_Role) string {
	trimmed := strings.TrimPrefix(in.String(), "ROLE_")
	split := camelcase.Split(trimmed)
	joined := strings.Join(split, "-")
	return strings.ToLower(joined)
}

func namespacePermissionName(in identityv1.NamespaceAccess_Permission) string {
	return strings.ToLower(strings.TrimPrefix(in.String(), "PERMISSION_"))
}

func getAccountRoleID(in identityv1.AccountAccess_Role, accountID string) string {
	return fmt.Sprintf("%s-%s", accountID, accountRoleName(in))
}

func accountRoleDisplayName(in identityv1.AccountAccess_Role) string {
	hr := humanReadableEnum("ROLE", in.String())
	return fmt.Sprintf("Account %s", hr)
}

func namespacePermissionDisplayName(in identityv1.NamespaceAccess_Permission, ns string) string {
	hr := humanReadableEnum("PERMISSION", in.String())
	return fmt.Sprintf("Namespace %s %s", ns, hr)
}

func humanReadableEnum(prefix string, s string) string {
	// APP_USER_TYPE_SERVICE_ACCOUNT -> app_user_type_service_account
	s = strings.TrimSpace(s)
	s = strings.ToLower(s)
	prefix = strings.ToLower(prefix)

	// app_user_type_service_account -> service_account
	s = strings.TrimPrefix(s, prefix)
	s = strings.TrimPrefix(s, "_")

	// service_account -> service account
	s = strings.ReplaceAll(s, "_", " ")

	// service account -> Service Account
	return cases.Title(language.AmericanEnglish).String(s)
}
