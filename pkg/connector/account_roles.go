package connector

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	"github.com/conductorone/baton-sdk/pkg/annotations"
	"github.com/conductorone/baton-sdk/pkg/pagination"
	"github.com/conductorone/baton-sdk/pkg/types/entitlement"
	"github.com/grpc-ecosystem/go-grpc-middleware/logging/zap/ctxzap"
	"github.com/temporalio/tcld/protogen/api/auth/v1"
	"github.com/temporalio/tcld/protogen/api/authservice/v1"
	"go.uber.org/zap"

	cloudservicev1 "github.com/conductorone/baton-temporalcloud/pkg/pb/temporal/api/cloud/cloudservice/v1"
)

const (
	AccountPermissionAssignmentMaxWaitDuration = 10 * time.Minute
)

const (
	roleMemberEntitlement = "member"
)

type accountRoleBuilder struct {
	client                cloudservicev1.CloudServiceClient
	authServiceClient     authservice.AuthServiceClient
	accountRoleCacheMutex sync.Mutex
	accountRolesCache     []*auth.Role
}

func (o *accountRoleBuilder) ResourceType(ctx context.Context) *v2.ResourceType {
	return accountRoleResourceType
}

func (o *accountRoleBuilder) getAccountRoles(ctx context.Context) ([]*auth.Role, error) {
	o.accountRoleCacheMutex.Lock()
	defer o.accountRoleCacheMutex.Unlock()
	if o.accountRolesCache != nil {
		return o.accountRolesCache, nil
	}

	req := &authservice.GetRolesByPermissionsRequest{Specs: []*auth.RoleSpec{
		{
			AccountRole: &auth.AccountRoleSpec{
				ActionGroup: auth.ACCOUNT_ACTION_GROUP_READ,
			},
		},
		{
			AccountRole: &auth.AccountRoleSpec{
				ActionGroup: auth.ACCOUNT_ACTION_GROUP_DEVELOPER,
			},
		},
		{
			AccountRole: &auth.AccountRoleSpec{
				ActionGroup: auth.ACCOUNT_ACTION_GROUP_ADMIN,
			},
		},
	}}

	resp, err := o.authServiceClient.GetRolesByPermissions(ctx, req)
	if err != nil {
		return nil, err
	}

	for _, role := range resp.GetRoles() {
		o.accountRolesCache = append(o.accountRolesCache, role)
	}

	return o.accountRolesCache, nil
}

func (o *accountRoleBuilder) List(ctx context.Context, _ *v2.ResourceId, _ *pagination.Token) ([]*v2.Resource, string, annotations.Annotations, error) {
	resp, err := o.getAccountRoles(ctx)
	if err != nil {
		return nil, "", nil, err
	}

	var rv []*v2.Resource
	for _, role := range resp {
		roleResource, err := protoAccountRoleToResource(role)
		if err != nil {
			return nil, "", nil, err
		}
		rv = append(rv, roleResource)
	}

	return rv, "", nil, nil
}

func (o *accountRoleBuilder) Entitlements(_ context.Context, r *v2.Resource, _ *pagination.Token) ([]*v2.Entitlement, string, annotations.Annotations, error) {
	annos := &v2.V1Identifier{
		Id: fmt.Sprintf("membership:%s", r.Id.Resource),
	}

	member := entitlement.NewAssignmentEntitlement(r, roleMemberEntitlement, entitlement.WithGrantableTo(userResourceType), entitlement.WithDescription(fmt.Sprintf("Has the %s role in Temporal Cloud", r.GetDisplayName())), entitlement.WithDisplayName(fmt.Sprintf("%s Role Member", r.GetDisplayName())), entitlement.WithAnnotation(annos))
	return []*v2.Entitlement{member}, "", nil, nil
}

func (o *accountRoleBuilder) Grants(ctx context.Context, r *v2.Resource, pToken *pagination.Token) ([]*v2.Grant, string, annotations.Annotations, error) {
	bag := &pagination.Bag{}
	err := bag.Unmarshal(pToken.Token)
	if err != nil {
		return nil, "", nil, err
	}
	if bag.Current() == nil {
		bag.Push(pagination.PageState{
			ResourceTypeID: r.Id.ResourceType,
			ResourceID:     r.Id.Resource,
		})
	}
	req := &authservice.GetUsersRequest{}
	if bag.PageToken() != "" {
		req.PageToken = bag.PageToken()
	}

	resp, err := o.authServiceClient.GetUsers(ctx, req)
	if err != nil {
		return nil, "", nil, err
	}

	var rv []*v2.Grant
	for _, user := range resp.GetUsers() {
		if !slices.Contains(user.GetSpec().GetRoles(), r.Id.Resource) {
			continue
		}
		grantResource, err := createAccountRoleGrant(user, r)
		if err != nil {
			return nil, "", nil, err
		}
		rv = append(rv, grantResource)
	}
	return paginate(rv, bag, resp.GetNextPageToken())
}

func (o *accountRoleBuilder) Grant(ctx context.Context, principal *v2.Resource, e *v2.Entitlement) ([]*v2.Grant, annotations.Annotations, error) {
	entitlementID := e.GetId()
	userID := principal.GetId().GetResource()
	userType := principal.GetId().GetResourceType()
	accountRole := e.GetResource()
	accountRoleID := accountRole.GetId().GetResource()
	accountRoleType := accountRole.GetId().GetResourceType()

	userResp, err := o.authServiceClient.GetUser(ctx, &authservice.GetUserRequest{UserId: userID})
	if err != nil {
		return nil, nil, fmt.Errorf("temporalcloud-connnector: couldn't retrieve user: %w", err)
	}

	var roleIDs []string
	var newRole *auth.Role
	roles, err := o.getAccountRoles(ctx)
	if err != nil {
		return nil, nil, err
	}
	for _, role := range roles {
		roleIDs = append(roleIDs, role.GetId())
		if accountRoleID == role.GetId() {
			newRole = role
		}
	}

	user := userResp.GetUser()
	spec := user.GetSpec()

	if strings.ToLower(newRole.GetSpec().GetAccountRole().GetActionGroup().String()) == "admin" {
		spec.Roles = []string{accountRoleID}
	} else {
		if slices.Contains(spec.GetRoles(), accountRoleID) {
			g, err := createAccountRoleGrant(user, accountRole)
			if err != nil {
				return nil, nil, err
			}
			return []*v2.Grant{g}, nil, nil
		}

		spec.Roles = slices.DeleteFunc(spec.GetRoles(), func(s string) bool {
			return slices.Contains(roleIDs, s)
		})
		spec.Roles = append(spec.GetRoles(), accountRoleID)
	}

	req := &authservice.UpdateUserRequest{UserId: userID, Spec: spec, ResourceVersion: userResp.GetUser().GetResourceVersion()}
	resp, err := o.authServiceClient.UpdateUser(ctx, req)
	if err != nil {
		return nil, nil, fmt.Errorf("temporalcloud-connector: could not grant entitlement to user: %w", err)
	}

	retryDelay := time.Duration(resp.GetRequestStatus().GetCheckDuration().GetSeconds()) * time.Second
	requestID := resp.GetRequestStatus().GetRequestId()
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
		return nil, nil, fmt.Errorf("temporalcloud-connector: account role assignment creation failed: %w", err)
	}

	annos := annotations.New()
	annos.Append(&v2.RequestId{RequestId: requestID})

	g, err := createAccountRoleGrant(user, accountRole)
	if err != nil {
		return nil, nil, err
	}

	return []*v2.Grant{g}, annos, nil
}

func (o *accountRoleBuilder) Revoke(ctx context.Context, g *v2.Grant) (annotations.Annotations, error) {
	e := g.GetEntitlement()
	principal := g.GetPrincipal()
	entitlementID := e.GetId()
	userID := principal.GetId().GetResource()
	userType := principal.GetId().GetResourceType()
	accountRole := e.GetResource()
	accountRoleID := accountRole.GetId().GetResource()
	accountRoleType := accountRole.GetId().GetResourceType()

	userResp, err := o.client.GetUser(ctx, &cloudservicev1.GetUserRequest{UserId: userID})
	if err != nil {
		return nil, fmt.Errorf("temporalcloud-connnector: couldn't retrieve user: %w", err)
	}

	req := &cloudservicev1.DeleteUserRequest{UserId: userID, ResourceVersion: userResp.GetUser().GetResourceVersion()}
	resp, err := o.client.DeleteUser(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("temporalcloud-connector: could not revoke entitlement for user: %w", err)
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
		return nil, fmt.Errorf("temporalcloud-connector: account role assignment deletion failed: %w", err)
	}

	annos := annotations.New()
	annos.Append(&v2.RequestId{RequestId: requestID})

	return annos, nil
}

func newAccountBuilder(client cloudservicev1.CloudServiceClient, authClient authservice.AuthServiceClient) *accountRoleBuilder {
	return &accountRoleBuilder{
		client:            client,
		authServiceClient: authClient,
	}
}
