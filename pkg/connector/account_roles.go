package connector

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	"github.com/conductorone/baton-sdk/pkg/annotations"
	"github.com/conductorone/baton-sdk/pkg/pagination"
	"github.com/conductorone/baton-sdk/pkg/types/entitlement"
	rs "github.com/conductorone/baton-sdk/pkg/types/resource"
	"github.com/grpc-ecosystem/go-grpc-middleware/logging/zap/ctxzap"
	identityv1 "go.temporal.io/cloud-sdk/api/identity/v1"
	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"

	cloudservicev1 "go.temporal.io/cloud-sdk/api/cloudservice/v1"

	"github.com/conductorone/baton-temporalcloud/pkg/client"
)

const (
	AccountPermissionAssignmentMaxWaitDuration = 10 * time.Minute
)

const (
	roleMemberEntitlement = "member"
)

var accountRoles = []identityv1.AccountAccess_Role{
	identityv1.AccountAccess_ROLE_OWNER,
	identityv1.AccountAccess_ROLE_ADMIN,
	identityv1.AccountAccess_ROLE_DEVELOPER,
	identityv1.AccountAccess_ROLE_FINANCE_ADMIN,
	identityv1.AccountAccess_ROLE_READ,
}

type accountRoleBuilder struct {
	client *client.Client
}

func (o *accountRoleBuilder) ResourceType(ctx context.Context) *v2.ResourceType {
	return accountRoleResourceType
}

func (o *accountRoleBuilder) List(ctx context.Context, _ *v2.ResourceId, _ rs.SyncOpAttrs) ([]*v2.Resource, *rs.SyncOpResults, error) {
	accountID, err := o.client.GetAccountID(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get account ID: %w", err)
	}
	var rv []*v2.Resource
	for _, role := range accountRoles {
		roleResource, err := protoAccountRoleToResource(role, accountID)
		if err != nil {
			return nil, nil, err
		}
		rv = append(rv, roleResource)
	}

	return rv, nil, nil
}

func (o *accountRoleBuilder) Entitlements(ctx context.Context, r *v2.Resource, _ rs.SyncOpAttrs) ([]*v2.Entitlement, *rs.SyncOpResults, error) {
	accountID, err := o.client.GetAccountID(ctx)
	if err != nil {
		return nil, nil, err
	}

	ar := AccountAccessRoleFromID(r.GetId().GetResource(), accountID)

	annos := []proto.Message{
		&v2.V1Identifier{
			Id: fmt.Sprintf("membership:%s", r.GetId().GetResource()),
		},
	}

	if slices.Contains(immutableAccountRoles, ar) {
		annos = append(annos, &v2.EntitlementImmutable{})
	}

	member := entitlement.NewAssignmentEntitlement(r, roleMemberEntitlement,
		entitlement.WithGrantableTo(userResourceType),
		entitlement.WithDescription(fmt.Sprintf("Has the %s role in Temporal Cloud", r.GetDisplayName())),
		entitlement.WithDisplayName(fmt.Sprintf("%s Role Member", r.GetDisplayName())),
		entitlement.WithAnnotation(annos...))
	return []*v2.Entitlement{member}, nil, nil
}

func (o *accountRoleBuilder) Grants(ctx context.Context, r *v2.Resource, opts rs.SyncOpAttrs) ([]*v2.Grant, *rs.SyncOpResults, error) {
	accountID, err := o.client.GetAccountID(ctx)
	if err != nil {
		return nil, nil, err
	}

	bag := &pagination.Bag{}
	err = bag.Unmarshal(opts.PageToken.Token)
	if err != nil {
		return nil, nil, err
	}
	if bag.Current() == nil {
		bag.Push(pagination.PageState{
			ResourceTypeID: r.Id.ResourceType,
			ResourceID:     r.Id.Resource,
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

	var rv []*v2.Grant
	for _, user := range resp.GetUsers() {
		if user.GetSpec().GetAccess().GetAccountAccess().GetRole() != AccountAccessRoleFromID(r.Id.Resource, accountID) {
			continue
		}
		grantResource, err := createAccountRoleGrant(user, r, accountID)
		if err != nil {
			return nil, nil, err
		}
		rv = append(rv, grantResource)
	}
	return paginate(rv, bag, resp.GetNextPageToken())
}

func (o *accountRoleBuilder) Grant(ctx context.Context, principal *v2.Resource, e *v2.Entitlement) ([]*v2.Grant, annotations.Annotations, error) {
	accountID, err := o.client.GetAccountID(ctx)
	if err != nil {
		return nil, nil, err
	}

	entitlementID := e.GetId()
	userID := principal.GetId().GetResource()
	userType := principal.GetId().GetResourceType()
	accountRole := e.GetResource()
	accountRoleID := accountRole.GetId().GetResource()
	accountRoleType := accountRole.GetId().GetResourceType()

	userResp, err := o.client.GetUser(ctx, &cloudservicev1.GetUserRequest{UserId: userID})
	if err != nil {
		return nil, nil, fmt.Errorf("baton-temporalcloud: couldn't retrieve user: %w", err)
	}

	currRole := userResp.GetUser().GetSpec().GetAccess().GetAccountAccess().GetRole()
	if slices.Contains(immutableAccountRoles, currRole) {
		zap.L().Info("baton-temporalcloud: user has immutable role, skipping grant", zap.String("user_id", userID))
		return nil, nil, nil
	}

	newRole := AccountAccessRoleFromID(accountRoleID, accountID)
	if newRole == identityv1.AccountAccess_ROLE_UNSPECIFIED {
		return nil, nil, fmt.Errorf("baton-temporalcloud: invalid account role %s", strings.TrimPrefix(accountRoleID, accountID+"-"))
	}

	if slices.Contains(immutableAccountRoles, newRole) {
		return nil, nil, fmt.Errorf("baton-temporalcloud: role %s is immutable and cannot be granted", accountRoleDisplayName(newRole))
	}

	user := userResp.GetUser()
	spec := user.GetSpec()

	newSpec := &identityv1.UserSpec{
		Email: spec.GetEmail(),
		Access: &identityv1.Access{
			NamespaceAccesses: spec.GetAccess().GetNamespaceAccesses(),
			AccountAccess: &identityv1.AccountAccess{
				Role: newRole,
			},
		},
	}

	req := &cloudservicev1.UpdateUserRequest{UserId: userID, Spec: newSpec, ResourceVersion: userResp.GetUser().GetResourceVersion()}
	resp, err := o.client.UpdateUser(ctx, req)
	if err != nil {
		if strings.Contains(err.Error(), "nothing to change") {
			return nil, annotations.New(&v2.GrantAlreadyExists{}), nil
		}

		return nil, nil, fmt.Errorf("baton-temporalcloud: could not grant entitlement to user: %w", err)
	}

	retryDelay := resp.GetAsyncOperation().GetCheckDuration().AsDuration()
	requestID := resp.GetAsyncOperation().GetId()
	l := ctxzap.Extract(ctx).With(
		zap.String("request_id", requestID),
		zap.String("principal_id", userID),
		zap.String("principal_type", userType),
		zap.String("entitlement_id", entitlementID),
		zap.String("entitlement_resource_id", accountRoleID),
		zap.String("entitlement_resource_type", accountRoleType),
	)
	waitCtx, cancel := context.WithTimeout(ctx, AccountPermissionAssignmentMaxWaitDuration)
	defer cancel()
	err = awaitAsyncOperation(waitCtx, l, o.client, requestID, retryDelay)
	if err != nil {
		return nil, nil, fmt.Errorf("baton-temporalcloud: account role assignment creation failed: %w", err)
	}

	annos := annotations.New()
	annos.Append(&v2.RequestId{RequestId: requestID})

	g, err := createAccountRoleGrant(user, accountRole, accountID)
	if err != nil {
		return nil, nil, err
	}

	return []*v2.Grant{g}, annos, nil
}

func (o *accountRoleBuilder) Revoke(ctx context.Context, g *v2.Grant) (annotations.Annotations, error) {
	accountID, err := o.client.GetAccountID(ctx)
	if err != nil {
		return nil, err
	}

	e := g.GetEntitlement()
	principal := g.GetPrincipal()
	entitlementID := e.GetId()
	userID := principal.GetId().GetResource()
	userType := principal.GetId().GetResourceType()
	accountRole := e.GetResource()
	accountRoleID := accountRole.GetId().GetResource()
	accountRoleType := accountRole.GetId().GetResourceType()

	ar := AccountAccessRoleFromID(accountRoleID, accountID)
	if slices.Contains(immutableAccountRoles, ar) {
		return nil, fmt.Errorf("baton-temporalcloud: role %s is immutable and cannot be revoked", accountRoleDisplayName(ar))
	}

	userResp, err := o.client.GetUser(ctx, &cloudservicev1.GetUserRequest{UserId: userID})
	if err != nil {
		return nil, fmt.Errorf("baton-temporalcloud: couldn't retrieve user: %w", err)
	}

	user := userResp.GetUser()

	var downgradedRole identityv1.AccountAccess_Role
	switch ar {
	case identityv1.AccountAccess_ROLE_ADMIN:
		downgradedRole = identityv1.AccountAccess_ROLE_DEVELOPER
	case identityv1.AccountAccess_ROLE_DEVELOPER:
		downgradedRole = identityv1.AccountAccess_ROLE_READ
	case identityv1.AccountAccess_ROLE_READ:
		return nil, fmt.Errorf("baton-temporalcloud: revoking %s role would delete the user account", identityv1.AccountAccess_ROLE_READ)
	default:
		return nil, fmt.Errorf("baton-temporalcloud: invalid account role %s", ar)
	}

	spec := user.GetSpec()

	if downgradedRole == spec.GetAccess().GetAccountAccess().GetRole() {
		annos := annotations.New()
		annos.Append(&v2.GrantAlreadyRevoked{})
		return annos, fmt.Errorf("baton-temporalcloud: user already has %s role", downgradedRole)
	}

	newSpec := &identityv1.UserSpec{
		Email: spec.GetEmail(),
		Access: &identityv1.Access{
			NamespaceAccesses: spec.GetAccess().GetNamespaceAccesses(),
			AccountAccess: &identityv1.AccountAccess{
				Role: downgradedRole,
			},
		},
	}

	req := &cloudservicev1.UpdateUserRequest{UserId: userID, Spec: newSpec, ResourceVersion: userResp.GetUser().GetResourceVersion()}
	resp, err := o.client.UpdateUser(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("baton-temporalcloud: could not revoke entitlement for user: %w", err)
	}

	retryDelay := resp.GetAsyncOperation().GetCheckDuration().AsDuration()
	requestID := resp.GetAsyncOperation().GetId()
	l := ctxzap.Extract(ctx).With(
		zap.String("request_id", requestID),
		zap.String("principal_id", userID),
		zap.String("principal_type", userType),
		zap.String("entitlement_id", entitlementID),
		zap.String("entitlement_resource_id", accountRoleID),
		zap.String("entitlement_resource_type", accountRoleType),
	)
	waitCtx, cancel := context.WithTimeout(ctx, AccountPermissionAssignmentMaxWaitDuration)
	defer cancel()
	err = awaitAsyncOperation(waitCtx, l, o.client, requestID, retryDelay)
	if err != nil {
		return nil, fmt.Errorf("baton-temporalcloud: account role assignment deletion failed: %w", err)
	}

	annos := annotations.New()
	annos.Append(&v2.RequestId{RequestId: requestID})

	return annos, nil
}

func newAccountBuilder(client *client.Client) *accountRoleBuilder {
	return &accountRoleBuilder{
		client: client,
	}
}
