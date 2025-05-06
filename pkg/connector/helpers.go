package connector

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"golang.org/x/text/cases"
	"golang.org/x/text/language"
	"google.golang.org/protobuf/proto"

	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	"github.com/conductorone/baton-sdk/pkg/annotations"
	"github.com/conductorone/baton-sdk/pkg/pagination"
	"github.com/conductorone/baton-sdk/pkg/types/grant"
	"github.com/conductorone/baton-sdk/pkg/types/resource"
	"github.com/fatih/camelcase"
	"go.uber.org/zap"

	cloudservicev1 "go.temporal.io/cloud-sdk/api/cloudservice/v1"
	identityv1 "go.temporal.io/cloud-sdk/api/identity/v1"
	namespacev1 "go.temporal.io/cloud-sdk/api/namespace/v1"
	operationv1 "go.temporal.io/cloud-sdk/api/operation/v1"
)

func protoUserToResource(proto *identityv1.User) (*v2.Resource, error) {
	annos := &v2.V1Identifier{
		Id: fmt.Sprintf("user:%s", proto.GetSpec().GetEmail()),
	}

	user, err := resource.NewUserResource(proto.GetSpec().GetEmail(), userResourceType, proto.GetId(), []resource.UserTraitOption{
		resource.WithEmail(proto.GetSpec().GetEmail(), true),
		resource.WithCreatedAt(proto.GetCreatedTime().AsTime()),
		resource.WithAccountType(v2.UserTrait_ACCOUNT_TYPE_HUMAN),
	}, resource.WithAnnotation(annos))
	if err != nil {
		return nil, err
	}
	return user, nil
}

func protoNamespaceToResource(proto *namespacev1.Namespace) (*v2.Resource, error) {
	annos := &v2.V1Identifier{
		Id: fmt.Sprintf("namespace:%s", proto.GetNamespace()),
	}

	ns, err := resource.NewResource(proto.GetNamespace(), namespaceResourceType, proto.GetNamespace(), resource.WithAnnotation(annos))
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
	role, err := resource.NewRoleResource(accountRoleDisplayName(proto), accountRoleResourceType, getAccountRoleID(proto, accountID), []resource.RoleTraitOption{}, resource.WithAnnotation(annos))
	if err != nil {
		return nil, err
	}
	return role, nil
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

	accountRole := accountAccessRoleFromID(ar.GetId().GetResource(), accountID)
	if slices.Contains(immutableAccountRoles, accountRole) {
		annos = append(annos, &v2.GrantImmutable{})
	}

	g := grant.NewGrant(ar, roleMemberEntitlement, ur.GetId(), grant.WithAnnotation(annos...))
	return g, nil
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

		l.Debug("temporalcloud-connector: waiting for operation to complete, checking status...")
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

func paginate[T any](rv T, bag *pagination.Bag, pageToken string) (T, string, annotations.Annotations, error) {
	if pageToken == "" {
		return rv, "", nil, nil
	}

	token, err := bag.NextToken(pageToken)
	if err != nil {
		return rv, "", nil, err
	}
	return rv, token, nil, nil
}

const (
	membershipEntitlementIDTemplate = "membership:%s"
	namespaceEntitlementIDTemplate  = "namespace:%s:%s"
	grantIDTemplate                 = "grant:%s:%s"
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

func accountAccessRoleFromString(in string) identityv1.AccountAccess_Role {
	var rv identityv1.AccountAccess_Role
	switch strings.ToLower(in) {
	case accountRoleName(identityv1.AccountAccess_ROLE_OWNER):
		rv = identityv1.AccountAccess_ROLE_OWNER
	case accountRoleName(identityv1.AccountAccess_ROLE_ADMIN):
		rv = identityv1.AccountAccess_ROLE_ADMIN
	case accountRoleName(identityv1.AccountAccess_ROLE_DEVELOPER):
		rv = identityv1.AccountAccess_ROLE_DEVELOPER
	case accountRoleName(identityv1.AccountAccess_ROLE_FINANCE_ADMIN):
		rv = identityv1.AccountAccess_ROLE_FINANCE_ADMIN
	case accountRoleName(identityv1.AccountAccess_ROLE_READ):
		rv = identityv1.AccountAccess_ROLE_READ
	default:
		rv = identityv1.AccountAccess_ROLE_UNSPECIFIED
	}
	return rv
}

func namespaceAccessPermissionFromString(in string) identityv1.NamespaceAccess_Permission {
	var rv identityv1.NamespaceAccess_Permission
	switch strings.ToLower(in) {
	case namespacePermissionName(identityv1.NamespaceAccess_PERMISSION_ADMIN):
		rv = identityv1.NamespaceAccess_PERMISSION_ADMIN
	case namespacePermissionName(identityv1.NamespaceAccess_PERMISSION_WRITE):
		rv = identityv1.NamespaceAccess_PERMISSION_WRITE
	case namespacePermissionName(identityv1.NamespaceAccess_PERMISSION_READ):
		rv = identityv1.NamespaceAccess_PERMISSION_READ
	default:
		rv = identityv1.NamespaceAccess_PERMISSION_UNSPECIFIED
	}
	return rv
}

func accountAccessRoleFromID(in string, accountID string) identityv1.AccountAccess_Role {
	if strings.HasSuffix(in, accountID) { // handle legacy admin role ID
		return identityv1.AccountAccess_ROLE_ADMIN
	}

	role := strings.TrimPrefix(in, accountID+"-")
	return accountAccessRoleFromString(role)
}

func accountRoleName(in identityv1.AccountAccess_Role) string {
	trimmed := strings.TrimPrefix(in.String(), "AccountAccessRole")
	split := camelcase.Split(trimmed)
	joined := strings.Join(split, "-")
	return strings.ToLower(joined)
}

func namespacePermissionName(in identityv1.NamespaceAccess_Permission) string {
	return strings.ToLower(strings.TrimPrefix(in.String(), "NamespaceAccessPermission"))
}

func getAccountRoleID(in identityv1.AccountAccess_Role, accountID string) string {
	return fmt.Sprintf("%s-%s", accountID, accountRoleName(in))
}

func accountRoleDisplayName(in identityv1.AccountAccess_Role) string {
	trimmed := strings.TrimPrefix(in.String(), "AccountAccessRole")
	split := camelcase.Split(trimmed)
	return fmt.Sprintf("Account %s", cases.Title(language.English).String(strings.Join(split, " ")))
}

func namespacePermissionDisplayName(in identityv1.NamespaceAccess_Permission, ns string) string {
	trimmed := strings.TrimPrefix(in.String(), "NamespaceAccessPermission")
	split := camelcase.Split(trimmed)
	return fmt.Sprintf("Namespace %s %s", ns, cases.Title(language.English).String(strings.Join(split, " ")))
}
