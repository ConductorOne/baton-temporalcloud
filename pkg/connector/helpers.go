package connector

import (
	"context"
	"fmt"
	"strings"
	"time"

	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	"github.com/conductorone/baton-sdk/pkg/types/grant"
	"github.com/conductorone/baton-sdk/pkg/types/resource"
	"go.uber.org/zap"

	cloudservicev1 "github.com/conductorone/baton-temporalcloud/pkg/pb/temporal/api/cloud/cloudservice/v1"
	identityv1 "github.com/conductorone/baton-temporalcloud/pkg/pb/temporal/api/cloud/identity/v1"
	namespacev1 "github.com/conductorone/baton-temporalcloud/pkg/pb/temporal/api/cloud/namespace/v1"
)

func protoUserToResource(proto *identityv1.User) (*v2.Resource, error) {
	annos := &v2.V1Identifier{
		Id: fmt.Sprintf("user:%s", proto.GetSpec().GetEmail()),
	}

	user, err := resource.NewUserResource(proto.GetSpec().GetEmail(), userResourceType, proto.Id, []resource.UserTraitOption{
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

func createNamespaceGrant(user *identityv1.User, namespace *v2.Resource, permission string) (*v2.Grant, error) {
	permission = strings.ToLower(permission)
	ur, err := protoUserToResource(user)
	if err != nil {
		return nil, err
	}
	annos := &v2.V1Identifier{
		Id: fmt.Sprintf("namespace-grant:%s:%s:%s", namespace.GetId().GetResource(), ur.GetId().GetResource(), permission),
	}
	g := grant.NewGrant(namespace, permission, ur.GetId(), grant.WithAnnotation(annos))
	g.Principal = ur
	return g, nil
}

func createAccountGrant(user *identityv1.User, account *v2.Resource, permission string) (*v2.Grant, error) {
	permission = strings.ToLower(permission)
	ur, err := protoUserToResource(user)
	if err != nil {
		return nil, err
	}
	annos := &v2.V1Identifier{
		Id: fmt.Sprintf("account-grand:%s:%s:%s", account.GetId().GetResource(), ur.GetId().GetResource(), permission),
	}
	g := grant.NewGrant(account, permission, ur.GetId(), grant.WithAnnotation(annos))
	g.Principal = ur

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
