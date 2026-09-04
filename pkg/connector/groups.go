package connector

import (
	"context"
	"fmt"
	"time"

	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	"github.com/conductorone/baton-sdk/pkg/annotations"
	"github.com/conductorone/baton-sdk/pkg/connectorbuilder"
	"github.com/conductorone/baton-sdk/pkg/pagination"
	"github.com/conductorone/baton-sdk/pkg/types/entitlement"
	"github.com/conductorone/baton-sdk/pkg/types/grant"
	rs "github.com/conductorone/baton-sdk/pkg/types/resource"
	"github.com/grpc-ecosystem/go-grpc-middleware/logging/zap/ctxzap"
	cloudservicev1 "go.temporal.io/cloud-sdk/api/cloudservice/v1"
	identityv1 "go.temporal.io/cloud-sdk/api/identity/v1"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

const (
	GroupMembershipMaxWaitDuration = 10 * time.Minute
)

var _ connectorbuilder.ResourceProvisionerV2 = (*groupBuilder)(nil)

type groupBuilder struct {
	client cloudservicev1.CloudServiceClient
}

func (o *groupBuilder) ResourceType(ctx context.Context) *v2.ResourceType {
	return groupResourceType
}

func (o *groupBuilder) List(ctx context.Context, _ *v2.ResourceId, opts rs.SyncOpAttrs) ([]*v2.Resource, *rs.SyncOpResults, error) {
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
		if status.Code(err) == codes.PermissionDenied {
			ctxzap.Extract(ctx).Warn("baton-temporalcloud: cannot list user groups with the current API key, skipping groups", zap.Error(err))
			return nil, nil, nil
		}
		return nil, nil, fmt.Errorf("baton-temporalcloud: failed to list user groups: %w", err)
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

func (o *groupBuilder) Entitlements(_ context.Context, r *v2.Resource, _ rs.SyncOpAttrs) ([]*v2.Entitlement, *rs.SyncOpResults, error) {
	options := []entitlement.EntitlementOption{
		entitlement.WithGrantableTo(userResourceType),
		entitlement.WithDisplayName(fmt.Sprintf("%s Group Member", r.GetDisplayName())),
		entitlement.WithDescription(fmt.Sprintf("Member of the %s user group in Temporal Cloud", r.GetDisplayName())),
	}

	if isImmutablyProvisionedGroup(r) {
		options = append(options, entitlement.WithAnnotation(&v2.EntitlementImmutable{}))
	}

	member := entitlement.NewAssignmentEntitlement(r, groupMemberEntitlement, options...)
	return []*v2.Entitlement{member}, nil, nil
}

func (o *groupBuilder) Grants(ctx context.Context, r *v2.Resource, opts rs.SyncOpAttrs) ([]*v2.Grant, *rs.SyncOpResults, error) {
	bag := &pagination.Bag{}
	err := bag.Unmarshal(opts.PageToken.Token)
	if err != nil {
		return nil, nil, err
	}

	if bag.Current() == nil {
		bag.Push(pagination.PageState{
			ResourceTypeID: r.GetId().GetResourceType(),
			ResourceID:     r.GetId().GetResource(),
		})
	}

	req := &cloudservicev1.GetUserGroupMembersRequest{
		GroupId: r.GetId().GetResource(),
	}
	if bag.PageToken() != "" {
		req.PageToken = bag.PageToken()
	}

	resp, err := o.client.GetUserGroupMembers(ctx, req)
	if err != nil {
		return nil, nil, fmt.Errorf("baton-temporalcloud: failed to list user group members: %w", err)
	}

	groupKind := groupKindFromResource(r)
	rv := make([]*v2.Grant, 0, len(resp.GetMembers()))
	for _, member := range resp.GetMembers() {
		userID := member.GetMemberId().GetUserId()
		if userID == "" {
			continue
		}

		principalID := &v2.ResourceId{
			ResourceType: userResourceType.Id,
			Resource:     userID,
		}

		annos := []proto.Message{
			&v2.V1Identifier{
				Id: grantID(membershipEntitlementID(r.GetId().GetResource()), userID),
			},
		}
		if groupKind != groupKindCloud && groupKind != "" {
			annos = append(annos, &v2.GrantImmutable{})
		}

		g := grant.NewGrant(r, groupMemberEntitlement, principalID, grant.WithAnnotation(annos...))
		rv = append(rv, g)
	}

	return paginate(rv, bag, resp.GetNextPageToken())
}

func (o *groupBuilder) Grant(ctx context.Context, principal *v2.Resource, e *v2.Entitlement) ([]*v2.Grant, annotations.Annotations, error) {
	groupID := e.GetResource().GetId().GetResource()
	userID := principal.GetId().GetResource()

	groupResp, err := o.client.GetUserGroup(ctx, &cloudservicev1.GetUserGroupRequest{GroupId: groupID})
	if err != nil {
		return nil, nil, fmt.Errorf("baton-temporalcloud: couldn't retrieve group: %w", err)
	}

	if kind := groupKindFromSpec(groupResp.GetGroup().GetSpec()); kind != groupKindCloud {
		return nil, nil, fmt.Errorf("baton-temporalcloud: %s groups are managed by the external identity provider and cannot be provisioned", kind)
	}

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
		if status.Code(err) == codes.AlreadyExists {
			return nil, annotations.New(&v2.GrantAlreadyExists{}), nil
		}
		return nil, nil, fmt.Errorf("baton-temporalcloud: could not add user to group: %w", err)
	}

	retryDelay := resp.GetAsyncOperation().GetCheckDuration().AsDuration()
	requestID := resp.GetAsyncOperation().GetId()
	l := ctxzap.Extract(ctx).With(
		zap.String("request_id", requestID),
		zap.String("principal_id", userID),
		zap.String("group_id", groupID),
	)
	waitCtx, cancel := context.WithTimeout(ctx, GroupMembershipMaxWaitDuration)
	defer cancel()
	err = awaitAsyncOperation(waitCtx, l, o.client, requestID, retryDelay)
	if err != nil {
		return nil, nil, fmt.Errorf("baton-temporalcloud: group membership creation failed: %w", err)
	}

	groupResource, err := protoUserGroupToResource(groupResp.GetGroup())
	if err != nil {
		return nil, nil, err
	}

	g := createUserGroupMemberGrant(groupResource, userID)

	annos := annotations.New()
	annos.Append(&v2.RequestId{RequestId: requestID})

	return []*v2.Grant{g}, annos, nil
}

func (o *groupBuilder) Revoke(ctx context.Context, g *v2.Grant) (annotations.Annotations, error) {
	e := g.GetEntitlement()
	groupID := e.GetResource().GetId().GetResource()
	userID := g.GetPrincipal().GetId().GetResource()

	resp, err := o.client.RemoveUserGroupMember(ctx, &cloudservicev1.RemoveUserGroupMemberRequest{
		GroupId: groupID,
		MemberId: &identityv1.UserGroupMemberId{
			MemberType: &identityv1.UserGroupMemberId_UserId{
				UserId: userID,
			},
		},
	})
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return annotations.New(&v2.GrantAlreadyRevoked{}), nil
		}
		return nil, fmt.Errorf("baton-temporalcloud: could not remove user from group: %w", err)
	}

	retryDelay := resp.GetAsyncOperation().GetCheckDuration().AsDuration()
	requestID := resp.GetAsyncOperation().GetId()
	l := ctxzap.Extract(ctx).With(
		zap.String("request_id", requestID),
		zap.String("principal_id", userID),
		zap.String("group_id", groupID),
	)
	waitCtx, cancel := context.WithTimeout(ctx, GroupMembershipMaxWaitDuration)
	defer cancel()
	err = awaitAsyncOperation(waitCtx, l, o.client, requestID, retryDelay)
	if err != nil {
		return nil, fmt.Errorf("baton-temporalcloud: group membership deletion failed: %w", err)
	}

	annos := annotations.New()
	annos.Append(&v2.RequestId{RequestId: requestID})

	return annos, nil
}

func newGroupBuilder(client cloudservicev1.CloudServiceClient) *groupBuilder {
	return &groupBuilder{client: client}
}

// createUserGroupMemberGrant builds a membership grant for a user in a group.
func createUserGroupMemberGrant(group *v2.Resource, userID string) *v2.Grant {
	annos := []proto.Message{
		&v2.V1Identifier{
			Id: grantID(membershipEntitlementID(group.GetId().GetResource()), userID),
		},
	}

	return grant.NewGrant(group, groupMemberEntitlement, &v2.ResourceId{
		ResourceType: userResourceType.Id,
		Resource:     userID,
	}, grant.WithAnnotation(annos...))
}

func groupKindFromResource(r *v2.Resource) string {
	profile := rs.GetProfile(r)
	if profile == nil {
		return ""
	}
	return profile.GetFields()[groupKindProfileKey].GetStringValue()
}

func isImmutablyProvisionedGroup(r *v2.Resource) bool {
	kind := groupKindFromResource(r)
	return kind == groupKindScim || kind == groupKindGoogle
}
