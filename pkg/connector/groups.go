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
)

const (
	GroupMembershipMaxDuration = 10 * time.Minute
)

type groupBuilder struct {
	client cloudservicev1.CloudServiceClient
}

func (o *groupBuilder) ResourceType(ctx context.Context) *v2.ResourceType {
	return groupResourceType
}

func (o *groupBuilder) List(ctx context.Context, parentResourceID *v2.ResourceId, opts rs.SyncOpAttrs) ([]*v2.Resource, *rs.SyncOpResults, error) {
	bag := &pagination.Bag{}
	err := bag.Unmarshal(opts.PageToken.Token)
	if err != nil {
		return nil, nil, err
	}

	if bag.Current() == nil {
		bag.Push(pagination.PageState{
			ResourceTypeID: groupResourceType.Id,
		})
	}

	req := &cloudservicev1.GetUserGroupsRequest{}
	if bag.PageToken() != "" {
		req.PageToken = bag.PageToken()
	}

	resp, err := o.client.GetUserGroups(ctx, req)
	if err != nil {
		return nil, nil, fmt.Errorf("baton-temporalcloud: failed to list groups: %w", err)
	}

	rv := make([]*v2.Resource, 0, len(resp.GetGroups()))
	for _, group := range resp.GetGroups() {
		groupResource, err := protoUserGroupToResource(group)
		if err != nil {
			return nil, nil, err
		}
		rv = append(rv, groupResource)
	}

	return paginate(rv, bag, resp.GetNextPageToken())
}

func (o *groupBuilder) Entitlements(_ context.Context, resource *v2.Resource, _ rs.SyncOpAttrs) ([]*v2.Entitlement, *rs.SyncOpResults, error) {
	annos := &v2.V1Identifier{
		Id: membershipEntitlementID(resource.GetId().GetResource()),
	}
	member := entitlement.NewAssignmentEntitlement(resource, roleMemberEntitlement,
		entitlement.WithGrantableTo(userResourceType),
		entitlement.WithDescription(fmt.Sprintf("Member of %s group in Temporal Cloud", resource.GetDisplayName())),
		entitlement.WithDisplayName(fmt.Sprintf("%s Group Member", resource.GetDisplayName())),
		entitlement.WithAnnotation(annos),
	)
	return []*v2.Entitlement{member}, nil, nil
}

func (o *groupBuilder) Grants(ctx context.Context, resource *v2.Resource, opts rs.SyncOpAttrs) ([]*v2.Grant, *rs.SyncOpResults, error) {
	bag := &pagination.Bag{}
	err := bag.Unmarshal(opts.PageToken.Token)
	if err != nil {
		return nil, nil, err
	}
	if bag.Current() == nil {
		bag.Push(pagination.PageState{
			ResourceTypeID: resource.GetId().GetResourceType(),
			ResourceID:     resource.GetId().GetResource(),
		})
	}

	req := &cloudservicev1.GetUserGroupMembersRequest{
		GroupId: resource.GetId().GetResource(),
	}
	if bag.PageToken() != "" {
		req.PageToken = bag.PageToken()
	}

	resp, err := o.client.GetUserGroupMembers(ctx, req)
	if err != nil {
		return nil, nil, fmt.Errorf("baton-temporalcloud: failed to list group members: %w", err)
	}

	var rv []*v2.Grant
	for _, member := range resp.GetMembers() {
		userID := member.GetMemberId().GetUserId()
		if userID == "" {
			continue
		}

		userResp, err := o.client.GetUser(ctx, &cloudservicev1.GetUserRequest{UserId: userID})
		if err != nil {
			return nil, nil, fmt.Errorf("baton-temporalcloud: failed to get user %s for group grant: %w", userID, err)
		}

		g, err := createGroupMemberGrant(userResp.GetUser(), resource)
		if err != nil {
			return nil, nil, err
		}
		rv = append(rv, g)
	}

	return paginate(rv, bag, resp.GetNextPageToken())
}

func (o *groupBuilder) Grant(ctx context.Context, principal *v2.Resource, e *v2.Entitlement) ([]*v2.Grant, annotations.Annotations, error) {
	userID := principal.GetId().GetResource()
	userType := principal.GetId().GetResourceType()
	groupResource := e.GetResource()
	groupID := groupResource.GetId().GetResource()
	groupType := groupResource.GetId().GetResourceType()
	entitlementID := e.GetId()

	req := &cloudservicev1.AddUserGroupMemberRequest{
		GroupId: groupID,
		MemberId: &identityv1.UserGroupMemberId{
			MemberType: &identityv1.UserGroupMemberId_UserId{
				UserId: userID,
			},
		},
	}

	resp, err := o.client.AddUserGroupMember(ctx, req)
	if err != nil {
		if strings.Contains(err.Error(), "already a member") || strings.Contains(err.Error(), "nothing to change") {
			return nil, annotations.New(&v2.GrantAlreadyExists{}), nil
		}
		return nil, nil, fmt.Errorf("baton-temporalcloud: could not add user to group: %w", err)
	}

	retryDelay := resp.GetAsyncOperation().GetCheckDuration().AsDuration()
	requestID := resp.GetAsyncOperation().GetId()
	l := ctxzap.Extract(ctx).With(
		zap.String("request_id", requestID),
		zap.String("principal_id", userID),
		zap.String("principal_type", userType),
		zap.String("entitlement_id", entitlementID),
		zap.String("entitlement_resource_id", groupID),
		zap.String("entitlement_resource_type", groupType),
	)
	waitCtx, cancel := context.WithTimeout(ctx, GroupMembershipMaxDuration)
	defer cancel()
	err = awaitAsyncOperation(waitCtx, l, o.client, requestID, retryDelay)
	if err != nil {
		return nil, nil, fmt.Errorf("baton-temporalcloud: group membership addition failed: %w", err)
	}

	userResp, err := o.client.GetUser(ctx, &cloudservicev1.GetUserRequest{UserId: userID})
	if err != nil {
		return nil, nil, fmt.Errorf("baton-temporalcloud: failed to get user after adding to group: %w", err)
	}

	g, err := createGroupMemberGrant(userResp.GetUser(), groupResource)
	if err != nil {
		return nil, nil, err
	}

	annos := annotations.New()
	annos.Append(&v2.RequestId{RequestId: requestID})

	return []*v2.Grant{g}, annos, nil
}

func (o *groupBuilder) Revoke(ctx context.Context, g *v2.Grant) (annotations.Annotations, error) {
	userID := g.GetPrincipal().GetId().GetResource()
	userType := g.GetPrincipal().GetId().GetResourceType()
	entitlementID := g.GetEntitlement().GetId()
	groupResource := g.GetEntitlement().GetResource()
	groupID := groupResource.GetId().GetResource()
	groupType := groupResource.GetId().GetResourceType()

	req := &cloudservicev1.RemoveUserGroupMemberRequest{
		GroupId: groupID,
		MemberId: &identityv1.UserGroupMemberId{
			MemberType: &identityv1.UserGroupMemberId_UserId{
				UserId: userID,
			},
		},
	}

	resp, err := o.client.RemoveUserGroupMember(ctx, req)
	if err != nil {
		if strings.Contains(err.Error(), "not a member") || strings.Contains(err.Error(), "nothing to change") {
			annos := annotations.New(&v2.GrantAlreadyRevoked{})
			return annos, fmt.Errorf("baton-temporalcloud: user is not a member of this group")
		}
		return nil, fmt.Errorf("baton-temporalcloud: could not remove user from group: %w", err)
	}

	retryDelay := resp.GetAsyncOperation().GetCheckDuration().AsDuration()
	requestID := resp.GetAsyncOperation().GetId()
	l := ctxzap.Extract(ctx).With(
		zap.String("request_id", requestID),
		zap.String("principal_id", userID),
		zap.String("principal_type", userType),
		zap.String("entitlement_id", entitlementID),
		zap.String("entitlement_resource_id", groupID),
		zap.String("entitlement_resource_type", groupType),
	)
	waitCtx, cancel := context.WithTimeout(ctx, GroupMembershipMaxDuration)
	defer cancel()
	err = awaitAsyncOperation(waitCtx, l, o.client, requestID, retryDelay)
	if err != nil {
		return nil, fmt.Errorf("baton-temporalcloud: group membership removal failed: %w", err)
	}

	annos := annotations.New()
	annos.Append(&v2.RequestId{RequestId: requestID})

	return annos, nil
}

func newGroupBuilder(client cloudservicev1.CloudServiceClient) *groupBuilder {
	return &groupBuilder{client: client}
}
