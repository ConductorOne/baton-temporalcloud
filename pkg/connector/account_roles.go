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
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	cloudservicev1 "go.temporal.io/cloud-sdk/api/cloudservice/v1"

	"github.com/conductorone/baton-temporalcloud/pkg/client"
)

const (
	AccountPermissionAssignmentMaxWaitDuration = 10 * time.Minute

	roleMemberEntitlement = "member"

	accountRolePhaseUsers  = "account-role-grants:users"
	accountRolePhaseGroups = "account-role-grants:groups"
)

var accountRoles = []identityv1.AccountAccess_Role{
	identityv1.AccountAccess_ROLE_OWNER,
	identityv1.AccountAccess_ROLE_ADMIN,
	identityv1.AccountAccess_ROLE_DEVELOPER,
	identityv1.AccountAccess_ROLE_FINANCE_ADMIN,
	identityv1.AccountAccess_ROLE_READ,
}

type accountRoleBuilder struct {
	client     *client.Client
	syncGroups bool
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
		entitlement.WithGrantableTo(userResourceType, groupResourceType),
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
			ResourceTypeID: accountRolePhaseUsers,
			ResourceID:     r.Id.Resource,
		})
		if o.syncGroups {
			bag.Push(pagination.PageState{
				ResourceTypeID: accountRolePhaseGroups,
				ResourceID:     r.Id.Resource,
			})
		}
	}

	var rv []*v2.Grant
	var nextPageToken string
	switch bag.ResourceTypeID() {
	case accountRolePhaseUsers:
		rv, nextPageToken, err = o.listUserAccountRoleGrants(ctx, r, accountID, bag)
	case accountRolePhaseGroups:
		rv, nextPageToken, err = o.listGroupAccountRoleGrants(ctx, r, accountID, bag)
	default:
		// Legacy page states from the previous connector version used the
		// resource type id as the state marker; treat as the users phase.
		rv, nextPageToken, err = o.listUserAccountRoleGrants(ctx, r, accountID, bag)
	}
	if err != nil {
		return nil, nil, err
	}

	return paginateGrants(rv, bag, nextPageToken)
}

func (o *accountRoleBuilder) listUserAccountRoleGrants(ctx context.Context, r *v2.Resource, accountID string, bag *pagination.Bag) ([]*v2.Grant, string, error) {
	req := &cloudservicev1.GetUsersRequest{}
	if bag.PageToken() != "" {
		req.PageToken = bag.PageToken()
	}

	resp, err := o.client.GetUsers(ctx, req)
	if err != nil {
		return nil, "", err
	}

	rv := make([]*v2.Grant, 0, len(resp.GetUsers()))
	for _, user := range resp.GetUsers() {
		if user.GetSpec().GetAccess().GetAccountAccess().GetRole() != AccountAccessRoleFromID(r.Id.Resource, accountID) {
			continue
		}
		grantResource, err := createAccountRoleGrant(user, r, accountID)
		if err != nil {
			return nil, "", err
		}
		rv = append(rv, grantResource)
	}
	return rv, resp.GetNextPageToken(), nil
}

func (o *accountRoleBuilder) listGroupAccountRoleGrants(ctx context.Context, r *v2.Resource, accountID string, bag *pagination.Bag) ([]*v2.Grant, string, error) {
	l := ctxzap.Extract(ctx)

	req := &cloudservicev1.GetUserGroupsRequest{}
	if bag.PageToken() != "" {
		req.PageToken = bag.PageToken()
	}

	resp, err := o.client.GetUserGroups(ctx, req)
	if err != nil {
		if status.Code(err) == codes.PermissionDenied {
			l.Warn("baton-temporalcloud: API key cannot list user groups; skipping group account-role grants", zap.String("role_id", r.GetId().GetResource()))
			return nil, "", nil
		}
		return nil, "", fmt.Errorf("baton-temporalcloud: failed to list user groups: %w", err)
	}

	role := AccountAccessRoleFromID(r.Id.Resource, accountID)
	rv := make([]*v2.Grant, 0, len(resp.GetGroups()))
	for _, group := range resp.GetGroups() {
		if group.GetSpec().GetAccess().GetAccountAccess().GetRole() != role {
			continue
		}
		groupResource, err := protoUserGroupToResource(group)
		if err != nil {
			return nil, "", err
		}
		rv = append(rv, newGroupAccountRoleGrant(groupResource, r, accountID))
	}
	return rv, resp.GetNextPageToken(), nil
}

func (o *accountRoleBuilder) Grant(ctx context.Context, principal *v2.Resource, e *v2.Entitlement) ([]*v2.Grant, annotations.Annotations, error) {
	accountID, err := o.client.GetAccountID(ctx)
	if err != nil {
		return nil, nil, err
	}

	if principal.GetId().GetResourceType() == groupResourceType.Id {
		return o.grantAccountRoleToGroup(ctx, principal, e, accountID)
	}

	return o.grantAccountRoleToUser(ctx, principal, e, accountID)
}

func (o *accountRoleBuilder) grantAccountRoleToUser(ctx context.Context, principal *v2.Resource, e *v2.Entitlement, accountID string) ([]*v2.Grant, annotations.Annotations, error) {
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

func (o *accountRoleBuilder) grantAccountRoleToGroup(ctx context.Context, principal *v2.Resource, e *v2.Entitlement, accountID string) ([]*v2.Grant, annotations.Annotations, error) {
	groupID := principal.GetId().GetResource()
	accountRole := e.GetResource()
	accountRoleID := accountRole.GetId().GetResource()

	newRole := AccountAccessRoleFromID(accountRoleID, accountID)
	if newRole == identityv1.AccountAccess_ROLE_UNSPECIFIED {
		return nil, nil, fmt.Errorf("baton-temporalcloud: invalid account role %s", strings.TrimPrefix(accountRoleID, accountID+"-"))
	}
	if slices.Contains(immutableAccountRoles, newRole) {
		return nil, nil, fmt.Errorf("baton-temporalcloud: role %s is immutable and cannot be granted to a group", accountRoleDisplayName(newRole))
	}

	groupResp, err := o.client.GetUserGroup(ctx, &cloudservicev1.GetUserGroupRequest{GroupId: groupID})
	if err != nil {
		return nil, nil, fmt.Errorf("baton-temporalcloud: couldn't retrieve group: %w", err)
	}

	group := groupResp.GetGroup()
	spec := group.GetSpec()

	currentRole := spec.GetAccess().GetAccountAccess().GetRole()
	if slices.Contains(immutableAccountRoles, currentRole) {
		ctxzap.Extract(ctx).Warn("baton-temporalcloud: group has immutable role, skipping grant", zap.String("group_id", groupID))
		return nil, nil, fmt.Errorf("baton-temporalcloud: cannot grant role to group %s: group holds immutable account role %s", groupID, accountRoleDisplayName(currentRole))
	}
	if currentRole == newRole {
		return nil, annotations.New(&v2.GrantAlreadyExists{}), nil
	}

	newSpec := &identityv1.UserGroupSpec{
		DisplayName: spec.GetDisplayName(),
		Access: &identityv1.Access{
			AccountAccess:     &identityv1.AccountAccess{Role: newRole},
			NamespaceAccesses: spec.GetAccess().GetNamespaceAccesses(),
		},
		GroupType: spec.GetGroupType(),
	}

	req := &cloudservicev1.UpdateUserGroupRequest{GroupId: groupID, Spec: newSpec, ResourceVersion: group.GetResourceVersion()}
	resp, err := o.client.UpdateUserGroup(ctx, req)
	if err != nil {
		if strings.Contains(err.Error(), "nothing to change") {
			return nil, annotations.New(&v2.GrantAlreadyExists{}), nil
		}
		return nil, nil, fmt.Errorf("baton-temporalcloud: could not grant entitlement to group: %w", err)
	}

	retryDelay := resp.GetAsyncOperation().GetCheckDuration().AsDuration()
	requestID := resp.GetAsyncOperation().GetId()
	l := ctxzap.Extract(ctx).With(
		zap.String("request_id", requestID),
		zap.String("group_id", groupID),
		zap.String("entitlement_resource_id", accountRoleID),
	)
	waitCtx, cancel := context.WithTimeout(ctx, AccountPermissionAssignmentMaxWaitDuration)
	defer cancel()
	err = awaitAsyncOperation(waitCtx, l, o.client, requestID, retryDelay)
	if err != nil {
		return nil, nil, fmt.Errorf("baton-temporalcloud: group account role assignment creation failed: %w", err)
	}

	annos := annotations.New()
	annos.Append(&v2.RequestId{RequestId: requestID})

	groupResource, err := protoUserGroupToResource(group)
	if err != nil {
		return nil, nil, err
	}

	g := newGroupAccountRoleGrant(groupResource, accountRole, accountID)
	return []*v2.Grant{g}, annos, nil
}

func (o *accountRoleBuilder) Revoke(ctx context.Context, g *v2.Grant) (annotations.Annotations, error) {
	accountID, err := o.client.GetAccountID(ctx)
	if err != nil {
		return nil, err
	}

	e := g.GetEntitlement()
	accountRole := e.GetResource()
	accountRoleID := accountRole.GetId().GetResource()

	ar := AccountAccessRoleFromID(accountRoleID, accountID)
	if slices.Contains(immutableAccountRoles, ar) {
		return nil, fmt.Errorf("baton-temporalcloud: role %s is immutable and cannot be revoked", accountRoleDisplayName(ar))
	}

	principal := g.GetPrincipal()
	if principal.GetId().GetResourceType() == groupResourceType.Id {
		return o.revokeAccountRoleFromGroup(ctx, principal, accountRoleID, ar)
	}

	return o.revokeAccountRoleFromUser(ctx, g, accountRoleID, ar, accountID)
}

func (o *accountRoleBuilder) revokeAccountRoleFromUser(ctx context.Context, g *v2.Grant, accountRoleID string, ar identityv1.AccountAccess_Role, accountID string) (annotations.Annotations, error) {
	e := g.GetEntitlement()
	principal := g.GetPrincipal()
	entitlementID := e.GetId()
	userID := principal.GetId().GetResource()
	userType := principal.GetId().GetResourceType()
	accountRoleType := e.GetResource().GetId().GetResourceType()

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

func (o *accountRoleBuilder) revokeAccountRoleFromGroup(ctx context.Context, principal *v2.Resource, accountRoleID string, ar identityv1.AccountAccess_Role) (annotations.Annotations, error) {
	groupID := principal.GetId().GetResource()
	groupType := principal.GetId().GetResourceType()

	groupResp, err := o.client.GetUserGroup(ctx, &cloudservicev1.GetUserGroupRequest{GroupId: groupID})
	if err != nil {
		return nil, fmt.Errorf("baton-temporalcloud: couldn't retrieve group: %w", err)
	}

	group := groupResp.GetGroup()
	spec := group.GetSpec()

	if spec.GetAccess().GetAccountAccess().GetRole() != ar {
		return annotations.New(&v2.GrantAlreadyRevoked{}), nil
	}

	var downgradedRole *identityv1.AccountAccess
	switch ar {
	case identityv1.AccountAccess_ROLE_ADMIN:
		downgradedRole = &identityv1.AccountAccess{Role: identityv1.AccountAccess_ROLE_DEVELOPER}
	case identityv1.AccountAccess_ROLE_DEVELOPER:
		downgradedRole = &identityv1.AccountAccess{Role: identityv1.AccountAccess_ROLE_READ}
	case identityv1.AccountAccess_ROLE_READ:
		downgradedRole = nil
	default:
		return nil, fmt.Errorf("baton-temporalcloud: invalid account role %s", ar)
	}

	newSpec := &identityv1.UserGroupSpec{
		DisplayName: spec.GetDisplayName(),
		Access: &identityv1.Access{
			AccountAccess:     downgradedRole,
			NamespaceAccesses: spec.GetAccess().GetNamespaceAccesses(),
		},
		GroupType: spec.GetGroupType(),
	}

	req := &cloudservicev1.UpdateUserGroupRequest{GroupId: groupID, Spec: newSpec, ResourceVersion: group.GetResourceVersion()}
	resp, err := o.client.UpdateUserGroup(ctx, req)
	if err != nil {
		if strings.Contains(err.Error(), "nothing to change") {
			return annotations.New(&v2.GrantAlreadyRevoked{}), nil
		}
		return nil, fmt.Errorf("baton-temporalcloud: could not revoke entitlement for group: %w", err)
	}

	retryDelay := resp.GetAsyncOperation().GetCheckDuration().AsDuration()
	requestID := resp.GetAsyncOperation().GetId()
	l := ctxzap.Extract(ctx).With(
		zap.String("request_id", requestID),
		zap.String("group_id", groupID),
		zap.String("group_type", groupType),
		zap.String("entitlement_resource_id", accountRoleID),
	)
	waitCtx, cancel := context.WithTimeout(ctx, AccountPermissionAssignmentMaxWaitDuration)
	defer cancel()
	err = awaitAsyncOperation(waitCtx, l, o.client, requestID, retryDelay)
	if err != nil {
		return nil, fmt.Errorf("baton-temporalcloud: group account role removal failed: %w", err)
	}

	annos := annotations.New()
	annos.Append(&v2.RequestId{RequestId: requestID})

	return annos, nil
}

func newAccountBuilder(client *client.Client, syncGroups bool) *accountRoleBuilder {
	return &accountRoleBuilder{
		client:     client,
		syncGroups: syncGroups,
	}
}
