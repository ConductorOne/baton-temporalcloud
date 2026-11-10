package connector

import (
	"context"

	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	"github.com/conductorone/baton-sdk/pkg/connectorbuilder"
	"github.com/conductorone/baton-sdk/pkg/pagination"
	rs "github.com/conductorone/baton-sdk/pkg/types/resource"

	cloudservicev1 "go.temporal.io/cloud-sdk/api/cloudservice/v1"
)

var _ connectorbuilder.ResourceSyncerV2 = (*serviceAccountBuilder)(nil)

type serviceAccountBuilder struct {
	client cloudservicev1.CloudServiceClient
}

func (o *serviceAccountBuilder) ResourceType(ctx context.Context) *v2.ResourceType {
	return serviceAccountResourceType
}

// List returns all service accounts from Temporal Cloud as resource objects.
// Service accounts are non-human identities and carry ACCOUNT_TYPE_SERVICE.
func (o *serviceAccountBuilder) List(ctx context.Context, parentResourceID *v2.ResourceId, opts rs.SyncOpAttrs) ([]*v2.Resource, *rs.SyncOpResults, error) {
	bag := &pagination.Bag{}
	err := bag.Unmarshal(opts.PageToken.Token)
	if err != nil {
		return nil, nil, err
	}

	if bag.Current() == nil {
		bag.Push(pagination.PageState{
			ResourceTypeID: serviceAccountResourceType.Id,
		})
	}

	req := &cloudservicev1.GetServiceAccountsRequest{}
	if bag.PageToken() != "" {
		req.PageToken = bag.PageToken()
	}

	resp, err := o.client.GetServiceAccounts(ctx, req)
	if err != nil {
		return nil, nil, err
	}

	rv := make([]*v2.Resource, 0, len(resp.GetServiceAccount()))
	for _, sa := range resp.GetServiceAccount() {
		saResource, err := protoServiceAccountToResource(sa)
		if err != nil {
			return nil, nil, err
		}
		rv = append(rv, saResource)
	}

	return paginate(rv, bag, resp.GetNextPageToken())
}

// Entitlements always returns an empty slice for service accounts.
func (o *serviceAccountBuilder) Entitlements(_ context.Context, resource *v2.Resource, _ rs.SyncOpAttrs) ([]*v2.Entitlement, *rs.SyncOpResults, error) {
	return nil, nil, nil
}

// Grants always returns an empty slice for service accounts since they don't have any entitlements.
func (o *serviceAccountBuilder) Grants(ctx context.Context, resource *v2.Resource, opts rs.SyncOpAttrs) ([]*v2.Grant, *rs.SyncOpResults, error) {
	return nil, nil, nil
}

func newServiceAccountBuilder(client cloudservicev1.CloudServiceClient) *serviceAccountBuilder {
	return &serviceAccountBuilder{client: client}
}
