package connector

import (
	"context"
	"fmt"
	"strings"
	"time"

	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	"github.com/conductorone/baton-sdk/pkg/annotations"
	"github.com/conductorone/baton-sdk/pkg/pagination"
	"github.com/conductorone/baton-sdk/pkg/types/grant"
	"github.com/conductorone/baton-sdk/pkg/types/resource"
	"github.com/temporalio/tcld/protogen/api/auth/v1"
	"go.uber.org/zap"

	cloudservicev1 "go.temporal.io/api/cloud/cloudservice/v1"
	identityv1 "go.temporal.io/api/cloud/identity/v1"
	namespacev1 "go.temporal.io/api/cloud/namespace/v1"
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

func authProtoUserToResource(proto *auth.User) (*v2.Resource, error) {
	annos := &v2.V1Identifier{
		Id: fmt.Sprintf("user:%s", proto.GetSpec().GetEmail()),
	}

	user, err := resource.NewUserResource(proto.GetSpec().GetEmail(), userResourceType, proto.GetId(), []resource.UserTraitOption{
		resource.WithEmail(proto.GetSpec().GetEmail(), true),
		resource.WithCreatedAt(time.Unix(proto.GetCreatedTime().GetSeconds(), int64(proto.GetCreatedTime().GetNanos()))),
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

func protoAccountRoleToResource(proto *auth.Role) (*v2.Resource, error) {
	ar := proto.GetSpec().GetAccountRole().GetActionGroup().String()
	annos := &v2.V1Identifier{
		Id: fmt.Sprintf("account-role:%s", strings.ToLower(ar)),
	}

	role, err := resource.NewRoleResource(fmt.Sprintf("Account %s", ar), accountRoleResourceType, proto.GetId(), []resource.RoleTraitOption{}, resource.WithAnnotation(annos))
	if err != nil {
		return nil, err
	}

	return role, nil
}

func createNamespaceGrant(user *identityv1.User, namespace *v2.Resource, permission string) (*v2.Grant, error) {
	permission = strings.ToLower(permission)
	ur, err := protoUserToResource(user)
	if err != nil {
		return nil, err
	}
	annos := &v2.V1Identifier{
		Id: grantID(namespaceEntitlementID(namespace.GetId().GetResource(), permission), ur.GetId().GetResource()),
	}
	g := grant.NewGrant(namespace, permission, ur.GetId(), grant.WithAnnotation(annos))
	g.Principal = ur
	return g, nil
}

func createAccountRoleGrant(user *auth.User, ar *v2.Resource) (*v2.Grant, error) {
	ur, err := authProtoUserToResource(user)
	if err != nil {
		return nil, err
	}
	annos := &v2.V1Identifier{
		Id: grantID(membershipEntitlementID(ar.GetId().GetResource()), ur.GetId().GetResource()),
	}

	g := grant.NewGrant(ar, roleMemberEntitlement, ur.GetId(), grant.WithAnnotation(annos))
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
	if op.GetFailureReason() != "" {
		return false, fmt.Errorf("operation failed: %s", op.GetFailureReason())
	}
	if strings.ToLower(op.State) == "fulfilled" {
		return true, nil
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
