package connector

import (
	"context"
	"fmt"
	"slices"
	"time"

	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	"github.com/conductorone/baton-sdk/pkg/annotations"
	"github.com/conductorone/baton-sdk/pkg/connectorbuilder"
	"github.com/conductorone/baton-sdk/pkg/pagination"
	rs "github.com/conductorone/baton-sdk/pkg/types/resource"
	"github.com/grpc-ecosystem/go-grpc-middleware/logging/zap/ctxzap"
	identityv1 "go.temporal.io/cloud-sdk/api/identity/v1"
	"go.uber.org/zap"

	cloudservicev1 "go.temporal.io/cloud-sdk/api/cloudservice/v1"
)

const (
	UserCreationMaxDuration = 10 * time.Minute
	UserDeletionMaxDuration = 10 * time.Minute
)

var _ connectorbuilder.AccountManagerV2 = (*userBuilder)(nil)

type AccountCreationSettings struct {
	DefaultAccountRole identityv1.AccountAccess_Role
}
type userBuilder struct {
	client                  cloudservicev1.CloudServiceClient
	accountCreationSettings AccountCreationSettings
}

func (o *userBuilder) ResourceType(ctx context.Context) *v2.ResourceType {
	return userResourceType
}

// List returns all the users from the database as resource objects.
// Users include a UserTrait because they are the 'shape' of a standard user.
func (o *userBuilder) List(ctx context.Context, parentResourceID *v2.ResourceId, opts rs.SyncOpAttrs) ([]*v2.Resource, *rs.SyncOpResults, error) {
	bag := &pagination.Bag{}
	err := bag.Unmarshal(opts.PageToken.Token)
	if err != nil {
		return nil, nil, err
	}

	if bag.Current() == nil {
		bag.Push(pagination.PageState{
			ResourceTypeID: userResourceType.Id,
		})
	}

	req := &cloudservicev1.GetUsersRequest{}
	if bag.PageToken() != "" {
		req.PageToken = bag.PageToken()
	}

	resp, err := o.client.GetUsers(ctx, req)
	if err != nil {
		return nil, nil, err
	}

	rv := make([]*v2.Resource, 0, len(resp.GetUsers()))
	for _, user := range resp.GetUsers() {
		userResource, err := protoUserToResource(user)
		if err != nil {
			return nil, nil, err
		}
		rv = append(rv, userResource)
	}

	return paginate(rv, bag, resp.NextPageToken)
}

// Entitlements always returns an empty slice for users.
func (o *userBuilder) Entitlements(_ context.Context, resource *v2.Resource, _ rs.SyncOpAttrs) ([]*v2.Entitlement, *rs.SyncOpResults, error) {
	return nil, nil, nil
}

// Grants always returns an empty slice for users since they don't have any entitlements.
func (o *userBuilder) Grants(ctx context.Context, resource *v2.Resource, opts rs.SyncOpAttrs) ([]*v2.Grant, *rs.SyncOpResults, error) {
	return nil, nil, nil
}

func (o *userBuilder) CreateAccountCapabilityDetails(_ context.Context) (*v2.CredentialDetailsAccountProvisioning, annotations.Annotations, error) {
	// No need to handle passwords as Temporal Cloud handles that through email invites
	return &v2.CredentialDetailsAccountProvisioning{
		SupportedCredentialOptions: []v2.CapabilityDetailCredentialOption{
			v2.CapabilityDetailCredentialOption_CAPABILITY_DETAIL_CREDENTIAL_OPTION_NO_PASSWORD,
		},
		PreferredCredentialOption: v2.CapabilityDetailCredentialOption_CAPABILITY_DETAIL_CREDENTIAL_OPTION_NO_PASSWORD,
	}, nil, nil
}

func (o *userBuilder) CreateAccount(
	ctx context.Context,
	accountInfo *v2.AccountInfo,
	credentialOptions *v2.LocalCredentialOptions,
) (connectorbuilder.CreateAccountResponse, []*v2.PlaintextData, annotations.Annotations, error) {
	l := ctxzap.Extract(ctx)
	profile := accountInfo.Profile.AsMap()

	email, ok := profile["email"].(string)
	if !ok || email == "" {
		return nil, nil, nil, fmt.Errorf("email is required")
	}

	req := &cloudservicev1.CreateUserRequest{
		Spec: &identityv1.UserSpec{
			Email: email,
			Access: &identityv1.Access{
				AccountAccess: &identityv1.AccountAccess{
					Role: o.accountCreationSettings.DefaultAccountRole,
				},
			},
		},
	}

	l.Info("baton-temporalcloud: creating user account")

	resp, err := o.client.CreateUser(ctx, req)
	if err != nil {
		l.Error("baton-temporalcloud: failed to create user account", zap.String("email", email), zap.Error(err))
		return nil, nil, nil, fmt.Errorf("failed to create user: %w", err)
	}

	retryDelay := resp.GetAsyncOperation().GetCheckDuration().AsDuration()
	requestID := resp.GetAsyncOperation().GetId()
	waitCtx, cancel := context.WithTimeout(ctx, UserCreationMaxDuration)
	defer cancel()
	err = awaitAsyncOperation(waitCtx, l.With(zap.String("request_id", requestID)), o.client, requestID, retryDelay)
	if err != nil {
		l.Error("baton-temporalcloud: user account creation failed", zap.String("email", email), zap.Error(err))
		return nil, nil, nil, fmt.Errorf("user account creation failed: %w", err)
	}

	l.Info("baton-temporalcloud: created Temporal Cloud user", zap.String("email", email))

	userResp, err := o.client.GetUser(ctx, &cloudservicev1.GetUserRequest{
		UserId: resp.GetUserId(),
	})
	if err != nil {
		l.Error("baton-temporalcloud: failed to get user account after creation", zap.String("email", email), zap.Error(err))
		return nil, nil, nil, fmt.Errorf("failed to get user account after creation: %w", err)
	}

	userRes, err := protoUserToResource(userResp.User)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to create user resource: %w", err)
	}

	successResult := &v2.CreateAccountResponse_SuccessResult{
		Resource: userRes,
	}

	l.Info("baton-temporalcloud: successfully created user account", zap.String("email", email), zap.String("user_id", resp.GetUserId()))

	return successResult, []*v2.PlaintextData{}, nil, nil
}

func (o *userBuilder) Delete(ctx context.Context, resourceID *v2.ResourceId) (annotations.Annotations, error) {
	if resourceID.ResourceType != userResourceType.Id {
		return nil, fmt.Errorf("baton-temporalcloud: non-user resource passed to user delete")
	}
	userID := resourceID.GetResource()
	userResp, err := o.client.GetUser(ctx, &cloudservicev1.GetUserRequest{
		UserId: userID,
	})
	if err != nil {
		return nil, fmt.Errorf("baton-temporalcloud: failed to retrieve user: %w", err)
	}
	accountRole := userResp.GetUser().GetSpec().GetAccess().GetAccountAccess().GetRole()
	if slices.Contains(immutableAccountRoles, accountRole) {
		return nil, fmt.Errorf("baton-temporacloud: cannot delete user with immutable %s role", accountRole.String())
	}

	req := &cloudservicev1.DeleteUserRequest{
		UserId:          userID,
		ResourceVersion: userResp.GetUser().GetResourceVersion(),
	}
	resp, err := o.client.DeleteUser(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("baton-temporalcloud: failed to delete user: %w", err)
	}
	retryDelay := resp.GetAsyncOperation().GetCheckDuration().AsDuration()
	requestID := resp.GetAsyncOperation().GetId()
	l := ctxzap.Extract(ctx).With(
		zap.String("request_id", requestID),
		zap.String("user_id", userID),
	)
	waitCtx, cancel := context.WithTimeout(ctx, UserDeletionMaxDuration)
	defer cancel()
	err = awaitAsyncOperation(waitCtx, l, o.client, requestID, retryDelay)
	if err != nil {
		return nil, fmt.Errorf("baton-temporalcloud: user account deletion failed: %w", err)
	}
	annos := annotations.New()
	annos.Append(&v2.RequestId{RequestId: requestID})
	return annos, nil
}

func newUserBuilder(client cloudservicev1.CloudServiceClient, accountCreationSettings AccountCreationSettings) *userBuilder {
	return &userBuilder{client: client, accountCreationSettings: accountCreationSettings}
}
