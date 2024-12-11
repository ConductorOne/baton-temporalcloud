package connector

import (
	"context"
	"io"

	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	"github.com/conductorone/baton-sdk/pkg/annotations"
	"github.com/conductorone/baton-sdk/pkg/connectorbuilder"

	"github.com/conductorone/baton-temporalcloud/pkg/client"
)

type Connector struct {
	cloudServiceClient *client.Client

	accountID string
}

// ResourceSyncers returns a ResourceSyncer for each resource type that should be synced from the upstream service.
func (d *Connector) ResourceSyncers(ctx context.Context) []connectorbuilder.ResourceSyncer {
	return []connectorbuilder.ResourceSyncer{
		newUserBuilder(d.cloudServiceClient),
		newNamespaceBuilder(d.cloudServiceClient),
		newAccountBuilder(d.cloudServiceClient),
	}
}

// Asset takes an input AssetRef and attempts to fetch it using the connector's authenticated http client
// It streams a response, always starting with a metadata object, following by chunked payloads for the asset.
func (d *Connector) Asset(ctx context.Context, asset *v2.AssetRef) (string, io.ReadCloser, error) {
	return "", nil, nil
}

// Metadata returns metadata about the connector.
func (d *Connector) Metadata(ctx context.Context) (*v2.ConnectorMetadata, error) {
	return &v2.ConnectorMetadata{
		DisplayName: "Temporal Cloud Baton Connector",
		Description: "A Baton connector for Temporal Cloud",
	}, nil
}

// Validate is called to ensure that the connector is properly configured. It should exercise any API credentials
// to be sure that they are valid.
func (d *Connector) Validate(ctx context.Context) (annotations.Annotations, error) {
	acctId, err := d.cloudServiceClient.GetAccountID(ctx)
	if err != nil {
		return nil, err
	}
	d.accountID = acctId
	return nil, nil
}

// New returns a new instance of the connector.
func New(ctx context.Context, apiKey string, allowInsecure bool) (*Connector, error) {
	var clientOpts []client.Opt
	if allowInsecure {
		clientOpts = append(clientOpts, client.AllowInsecure())
	}

	c, err := client.New(apiKey, clientOpts...)
	if err != nil {
		return nil, err
	}

	return &Connector{
		cloudServiceClient: c,
	}, nil
}
