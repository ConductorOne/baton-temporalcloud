package connector

import (
	"context"
	"fmt"
	"strings"
	"time"

	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	"github.com/conductorone/baton-sdk/pkg/annotations"
	"github.com/conductorone/baton-sdk/pkg/pagination"
	"github.com/conductorone/baton-sdk/pkg/types/entitlement"
	rs "github.com/conductorone/baton-sdk/pkg/types/resource"
	"github.com/grpc-ecosystem/go-grpc-middleware/logging/zap/ctxzap"
	cloudservicev1 "go.temporal.io/cloud-sdk/api/cloudservice/v1"
	identityv1 "go.temporal.io/cloud-sdk/api/identity/v1"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	NamespacePermissionAssignmentMaxDuration = 10 * time.Minute

	namespacePhaseUsers  = "namespace-grants:users"
	namespacePhaseGroups = "namespace-grants:groups"
)

var namespaceAccessLevels = []identityv1.NamespaceAccess_Permission{
	identityv1.NamespaceAccess_PERMISSION_ADMIN,
	identityv1.NamespaceAccess_PERMISSION_WRITE,
	identityv1.NamespaceAccess_PERMISSION_READ,
}

type namespaceBuilder struct {
	client cloudservicev1.CloudServiceClient
}

func (o *namespaceBuilder) ResourceType(ctx context.Context) *v2.ResourceType {
	return namespaceResourceType
}

func (o *namespaceBuilder) List(ctx context.Context, parentResourceID *v2.ResourceId, opts rs.SyncOpAttrs) ([]*v2.Resource, *rs.SyncOpResults, error) {
	bag := &pagination.Bag{}
	err := bag.Unmarshal(opts.PageToken.Token)
	if err != nil {
		return nil, nil, err
	}

	if bag.Current() == nil {
		bag.Push(pagination.PageState{
			ResourceTypeID: namespaceResourceType.Id,
		})
	}

	req := &cloudservicev1.GetNamespacesRequest{}
	if bag.PageToken() != "" {
		req.PageToken = bag.PageToken()
	}

	resp, err := o.client.GetNamespaces(ctx, req)
	if err != nil {
		return nil, nil, err
	}

	rv := make([]*v2.Resource, 0, len(resp.GetNamespaces()))
	for _, namespace := range resp.GetNamespaces() {
		nsResource, err := protoNamespaceToResource(namespace)
		if err != nil {
			return nil, nil, err
		}
		rv = append(rv, nsResource)
	}

	return paginate(rv, bag, resp.GetNextPageToken())
}

func (o *namespaceBuilder) Entitlements(_ context.Context, resource *v2.Resource, _ rs.SyncOpAttrs) ([]*v2.Entitlement, *rs.SyncOpResults, error) {
	rv := make([]*v2.Entitlement, 0, len(namespaceAccessLevels))
	for _, level := range namespaceAccessLevels {
		annos := &v2.V1Identifier{
			Id: namespaceEntitlementID(resource.GetId().GetResource(), namespacePermissionName(level)),
		}
		e := entitlement.NewPermissionEntitlement(
			resource, namespacePermissionName(level),
			entitlement.WithDisplayName(namespacePermissionDisplayName(level, resource.GetDisplayName())),
			entitlement.WithDescription(fmt.Sprintf("Access to %s namespace in Temporal Cloud", resource.GetDisplayName())),
			entitlement.WithAnnotation(annos),
			entitlement.WithGrantableTo(userResourceType, groupResourceType),
		)
		rv = append(rv, e)
	}

	return rv, nil, nil
}

func (o *namespaceBuilder) Grants(ctx context.Context, resource *v2.Resource, opts rs.SyncOpAttrs) ([]*v2.Grant, *rs.SyncOpResults, error) {
	bag := &pagination.Bag{}
	err := bag.Unmarshal(opts.PageToken.Token)
	if err != nil {
		return nil, nil, err
	}
	if bag.Current() == nil {
		bag.Push(pagination.PageState{
			ResourceTypeID: namespacePhaseUsers,
			ResourceID:     resource.GetId().GetResource(),
		})
		bag.Push(pagination.PageState{
			ResourceTypeID: namespacePhaseGroups,
			ResourceID:     resource.GetId().GetResource(),
		})
	}

	var rv []*v2.Grant
	var nextPageToken string
	switch bag.ResourceTypeID() {
	case namespacePhaseUsers:
		rv, nextPageToken, err = o.listUserNamespaceGrants(ctx, resource, bag)
	case namespacePhaseGroups:
		rv, nextPageToken, err = o.listGroupNamespaceGrants(ctx, resource, bag)
	default:
		return nil, nil, fmt.Errorf("baton-temporalcloud: unexpected namespace grants pagination phase %q", bag.ResourceTypeID())
	}
	if err != nil {
		return nil, nil, err
	}

	return paginateGrants(rv, bag, nextPageToken)
}

func (o *namespaceBuilder) listUserNamespaceGrants(ctx context.Context, resource *v2.Resource, bag *pagination.Bag) ([]*v2.Grant, string, error) {
	req := &cloudservicev1.GetUsersRequest{Namespace: resource.GetDisplayName()}
	if bag.PageToken() != "" {
		req.PageToken = bag.PageToken()
	}

	resp, err := o.client.GetUsers(ctx, req)
	if err != nil {
		return nil, "", err
	}

	var rv []*v2.Grant
	for _, user := range resp.GetUsers() {
		permission, hasPerm := user.GetSpec().GetAccess().GetNamespaceAccesses()[resource.GetId().GetResource()]
		if !hasPerm {
			continue
		}

		g, err := createNamespaceGrant(user, resource, permission.GetPermission())
		if err != nil {
			return nil, "", err
		}
		rv = append(rv, g)
	}

	return rv, resp.GetNextPageToken(), nil
}

func (o *namespaceBuilder) listGroupNamespaceGrants(ctx context.Context, resource *v2.Resource, bag *pagination.Bag) ([]*v2.Grant, string, error) {
	l := ctxzap.Extract(ctx)
	nsID := resource.GetId().GetResource()

	req := &cloudservicev1.GetUserGroupsRequest{Namespace: resource.GetDisplayName()}
	if bag.PageToken() != "" {
		req.PageToken = bag.PageToken()
	}

	resp, err := o.client.GetUserGroups(ctx, req)
	if err != nil {
		if status.Code(err) == codes.PermissionDenied {
			l.Warn("baton-temporalcloud: API key cannot list user groups; skipping group namespace grants", zap.String("namespace_id", nsID))
			return nil, "", nil
		}
		return nil, "", fmt.Errorf("baton-temporalcloud: failed to list user groups: %w", err)
	}

	rv := make([]*v2.Grant, 0, len(resp.GetGroups()))
	for _, group := range resp.GetGroups() {
		nsAccess, ok := group.GetSpec().GetAccess().GetNamespaceAccesses()[nsID]
		if !ok {
			continue
		}
		groupResource, err := protoUserGroupToResource(group)
		if err != nil {
			return nil, "", err
		}
		rv = append(rv, newGroupNamespaceGrant(groupResource, resource, nsAccess.GetPermission()))
	}

	return rv, resp.GetNextPageToken(), nil
}

func (o *namespaceBuilder) Grant(ctx context.Context, principal *v2.Resource, e *v2.Entitlement) ([]*v2.Grant, annotations.Annotations, error) {
	entitlementID := e.GetId()
	namespace := e.GetResource()
	namespaceID := namespace.GetId().GetResource()
	namespaceType := namespace.GetId().GetResourceType()

	enIDParts := strings.Split(entitlementID, ":")
	if len(enIDParts) != 3 {
		return nil, nil, fmt.Errorf("baton-temporalcloud: invalid entitlement ID %s", entitlementID)
	}

	nsRole := enIDParts[2]

	namespaceRole := namespaceAccessPermissionFromString(nsRole)
	if namespaceRole == identityv1.NamespaceAccess_PERMISSION_UNSPECIFIED {
		return nil, nil, fmt.Errorf("baton-temporalcloud: invalid namespace permission %s", nsRole)
	}

	principalID := principal.GetId().GetResource()
	principalType := principal.GetId().GetResourceType()

	if principalType == groupResourceType.Id {
		return o.grantNamespacePermissionToGroup(ctx, principalID, namespace, namespaceRole, namespaceType)
	}
	if principalType == userResourceType.Id {
		return o.grantNamespacePermissionToUser(ctx, principalID, principalType, namespace, namespaceID, namespaceRole, namespaceType, entitlementID)
	}
	return nil, nil, fmt.Errorf("baton-temporalcloud: unsupported principal type %s", principalType)
}

func (o *namespaceBuilder) grantNamespacePermissionToUser(
	ctx context.Context, userID string, userType string,
	namespace *v2.Resource, namespaceID string,
	namespaceRole identityv1.NamespaceAccess_Permission,
	namespaceType string, entitlementID string,
) ([]*v2.Grant, annotations.Annotations, error) {
	userResp, err := o.client.GetUser(ctx, &cloudservicev1.GetUserRequest{UserId: userID})
	if err != nil {
		return nil, nil, fmt.Errorf("baton-temporalcloud: couldn't retrieve user: %w", err)
	}
	user := userResp.GetUser()
	spec := user.GetSpec()
	perm := &identityv1.NamespaceAccess{Permission: namespaceRole}
	ns := spec.GetAccess().GetNamespaceAccesses()
	if ns == nil {
		ns = map[string]*identityv1.NamespaceAccess{
			namespaceID: perm,
		}
	} else {
		existing, ok := ns[namespaceID]
		if ok && existing.GetPermission() == namespaceRole {
			return nil, annotations.New(&v2.GrantAlreadyExists{}), nil
		}
		ns[namespaceID] = perm
	}
	spec.Access.NamespaceAccesses = ns

	req := &cloudservicev1.UpdateUserRequest{UserId: userID, Spec: spec, ResourceVersion: user.GetResourceVersion()}
	resp, err := o.client.UpdateUser(ctx, req)
	if err != nil {
		if strings.Contains(err.Error(), "nothing to change") {
			return nil, annotations.New(&v2.GrantAlreadyExists{}), nil
		}
		return nil, nil, fmt.Errorf("baton-temporalcloud: could not grant namespace permission to user: %w", err)
	}

	retryDelay := resp.GetAsyncOperation().GetCheckDuration().AsDuration()
	requestID := resp.GetAsyncOperation().GetId()
	l := ctxzap.Extract(ctx).With(
		zap.String("request_id", requestID),
		zap.String("principal_id", userID),
		zap.String("principal_type", userType),
		zap.String("entitlement_id", entitlementID),
		zap.String("entitlement_resource_id", namespaceID),
		zap.String("entitlement_resource_type", namespaceType),
	)
	waitCtx, cancel := context.WithTimeout(ctx, NamespacePermissionAssignmentMaxDuration)
	defer cancel()
	err = awaitAsyncOperation(waitCtx, l, o.client, requestID, retryDelay)
	if err != nil {
		return nil, nil, fmt.Errorf("baton-temporalcloud: namespace permission grant to user failed: %w", err)
	}

	g, err := createNamespaceGrant(user, namespace, namespaceRole)
	if err != nil {
		return nil, nil, err
	}

	annos := annotations.New()
	annos.Append(&v2.RequestId{RequestId: requestID})

	return []*v2.Grant{g}, annos, nil
}

func (o *namespaceBuilder) grantNamespacePermissionToGroup(
	ctx context.Context, groupID string,
	namespace *v2.Resource, namespaceRole identityv1.NamespaceAccess_Permission,
	namespaceType string,
) ([]*v2.Grant, annotations.Annotations, error) {
	groupResp, err := o.client.GetUserGroup(ctx, &cloudservicev1.GetUserGroupRequest{GroupId: groupID})
	if err != nil {
		return nil, nil, fmt.Errorf("baton-temporalcloud: couldn't retrieve group: %w", err)
	}

	group := groupResp.GetGroup()
	namespaceID := namespace.GetId().GetResource()
	existing := group.GetSpec().GetAccess().GetNamespaceAccesses()[namespaceID]
	if existing != nil && existing.GetPermission() == namespaceRole {
		return nil, annotations.New(&v2.GrantAlreadyExists{}), nil
	}

	resp, err := o.client.SetUserGroupNamespaceAccess(ctx, &cloudservicev1.SetUserGroupNamespaceAccessRequest{
		Namespace:       namespaceID,
		GroupId:         groupID,
		Access:          &identityv1.NamespaceAccess{Permission: namespaceRole},
		ResourceVersion: group.GetResourceVersion(),
	})
	if err != nil {
		if strings.Contains(err.Error(), "nothing to change") {
			return nil, annotations.New(&v2.GrantAlreadyExists{}), nil
		}
		return nil, nil, fmt.Errorf("baton-temporalcloud: could not grant namespace permission to group: %w", err)
	}

	retryDelay := resp.GetAsyncOperation().GetCheckDuration().AsDuration()
	requestID := resp.GetAsyncOperation().GetId()
	l := ctxzap.Extract(ctx).With(
		zap.String("request_id", requestID),
		zap.String("group_id", groupID),
		zap.String("namespace_id", namespaceID),
		zap.String("entitlement_resource_type", namespaceType),
	)
	waitCtx, cancel := context.WithTimeout(ctx, NamespacePermissionAssignmentMaxDuration)
	defer cancel()
	err = awaitAsyncOperation(waitCtx, l, o.client, requestID, retryDelay)
	if err != nil {
		return nil, nil, fmt.Errorf("baton-temporalcloud: namespace permission grant to group failed: %w", err)
	}

	groupResource, err := protoUserGroupToResource(group)
	if err != nil {
		return nil, nil, err
	}

	g := newGroupNamespaceGrant(groupResource, namespace, namespaceRole)

	annos := annotations.New()
	annos.Append(&v2.RequestId{RequestId: requestID})

	return []*v2.Grant{g}, annos, nil
}

func (o *namespaceBuilder) Revoke(ctx context.Context, g *v2.Grant) (annotations.Annotations, error) {
	principal := g.GetPrincipal()
	principalID := principal.GetId().GetResource()
	principalType := principal.GetId().GetResourceType()
	entitlementID := g.GetEntitlement().GetId()
	namespace := g.GetEntitlement().GetResource()
	namespaceID := namespace.GetId().GetResource()
	namespaceType := namespace.GetId().GetResourceType()

	if principalType == groupResourceType.Id {
		return o.revokeNamespaceAccessFromGroup(ctx, g, principalID, namespaceID, namespaceType, entitlementID)
	}

	if principalType == userResourceType.Id {
		return o.revokeNamespaceAccessFromUser(ctx, g, principalID, principalType, namespaceID, namespaceType, entitlementID)
	}
	return nil, fmt.Errorf("baton-temporalcloud: unsupported principal type %s", principalType)
}

func (o *namespaceBuilder) revokeNamespaceAccessFromUser(
	ctx context.Context, _ *v2.Grant, userID string, userType string,
	namespaceID string, namespaceType string, entitlementID string,
) (annotations.Annotations, error) {
	userResp, err := o.client.GetUser(ctx, &cloudservicev1.GetUserRequest{UserId: userID})
	if err != nil {
		return nil, fmt.Errorf("baton-temporalcloud: couldn't retrieve user: %w", err)
	}
	user := userResp.GetUser()
	spec := user.GetSpec()
	_, ok := spec.GetAccess().GetNamespaceAccesses()[namespaceID]
	if !ok {
		return annotations.New(&v2.GrantAlreadyRevoked{}), nil
	}

	delete(spec.Access.NamespaceAccesses, namespaceID)
	req := &cloudservicev1.UpdateUserRequest{UserId: userID, Spec: spec, ResourceVersion: user.GetResourceVersion()}
	resp, err := o.client.UpdateUser(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("baton-temporalcloud: could not revoke namespace permission from user: %w", err)
	}

	retryDelay := resp.GetAsyncOperation().GetCheckDuration().AsDuration()
	requestID := resp.GetAsyncOperation().GetId()
	l := ctxzap.Extract(ctx).With(
		zap.String("request_id", requestID),
		zap.String("principal_id", userID),
		zap.String("principal_type", userType),
		zap.String("entitlement_id", entitlementID),
		zap.String("entitlement_resource_id", namespaceID),
		zap.String("entitlement_resource_type", namespaceType),
	)
	waitCtx, cancel := context.WithTimeout(ctx, NamespacePermissionAssignmentMaxDuration)
	defer cancel()
	err = awaitAsyncOperation(waitCtx, l, o.client, requestID, retryDelay)
	if err != nil {
		return nil, fmt.Errorf("baton-temporalcloud: namespace permission revoke for user failed: %w", err)
	}

	annos := annotations.New()
	annos.Append(&v2.RequestId{RequestId: requestID})

	return annos, nil
}

func (o *namespaceBuilder) revokeNamespaceAccessFromGroup(
	ctx context.Context, _ *v2.Grant, groupID string,
	namespaceID string, namespaceType string, entitlementID string,
) (annotations.Annotations, error) {
	groupResp, err := o.client.GetUserGroup(ctx, &cloudservicev1.GetUserGroupRequest{GroupId: groupID})
	if err != nil {
		return nil, fmt.Errorf("baton-temporalcloud: couldn't retrieve group: %w", err)
	}

	group := groupResp.GetGroup()
	_, ok := group.GetSpec().GetAccess().GetNamespaceAccesses()[namespaceID]
	if !ok {
		return annotations.New(&v2.GrantAlreadyRevoked{}), nil
	}

	resp, err := o.client.SetUserGroupNamespaceAccess(ctx, &cloudservicev1.SetUserGroupNamespaceAccessRequest{
		Namespace:       namespaceID,
		GroupId:         groupID,
		Access:          nil,
		ResourceVersion: group.GetResourceVersion(),
	})
	if err != nil {
		return nil, fmt.Errorf("baton-temporalcloud: could not revoke namespace permission from group: %w", err)
	}

	retryDelay := resp.GetAsyncOperation().GetCheckDuration().AsDuration()
	requestID := resp.GetAsyncOperation().GetId()
	l := ctxzap.Extract(ctx).With(
		zap.String("request_id", requestID),
		zap.String("group_id", groupID),
		zap.String("entitlement_id", entitlementID),
		zap.String("entitlement_resource_id", namespaceID),
		zap.String("entitlement_resource_type", namespaceType),
	)
	waitCtx, cancel := context.WithTimeout(ctx, NamespacePermissionAssignmentMaxDuration)
	defer cancel()
	err = awaitAsyncOperation(waitCtx, l, o.client, requestID, retryDelay)
	if err != nil {
		return nil, fmt.Errorf("baton-temporalcloud: namespace permission revoke for group failed: %w", err)
	}

	annos := annotations.New()
	annos.Append(&v2.RequestId{RequestId: requestID})

	return annos, nil
}

func newNamespaceBuilder(client cloudservicev1.CloudServiceClient) *namespaceBuilder {
	return &namespaceBuilder{client: client}
}
