package connector

import (
	"context"
	"fmt"
	"io"
	"slices"

	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	"github.com/conductorone/baton-sdk/pkg/annotations"
	"github.com/conductorone/baton-sdk/pkg/connectorbuilder"
	identityv1 "go.temporal.io/cloud-sdk/api/identity/v1"

	"github.com/conductorone/baton-temporalcloud/pkg/client"
)

type Connector struct {
	cloudServiceClient *client.Client

	accountID string

	accountCreationSettings AccountCreationSettings
}

// ResourceSyncers returns a ResourceSyncer for each resource type that should be synced from the upstream service.
func (d *Connector) ResourceSyncers(ctx context.Context) []connectorbuilder.ResourceSyncer {
	return []connectorbuilder.ResourceSyncer{
		newUserBuilder(d.cloudServiceClient, d.accountCreationSettings),
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
		AccountCreationSchema: &v2.ConnectorAccountCreationSchema{
			FieldMap: map[string]*v2.ConnectorAccountCreationSchema_Field{
				"email": {
					DisplayName: "Email",
					Required:    true,
					Description: "The email address of the user.",
					Field: &v2.ConnectorAccountCreationSchema_Field_StringField{
						StringField: &v2.ConnectorAccountCreationSchema_StringField{},
					},
					Placeholder: "user@example.com",
					Order:       1,
				},
			},
		},
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

	if d.accountCreationSettings.DefaultAccountRole == identityv1.AccountAccess_ROLE_UNSPECIFIED {
		return nil, fmt.Errorf("must specify a default account role")
	}

	if slices.Contains(immutableAccountRoles, d.accountCreationSettings.DefaultAccountRole) {
		return nil, fmt.Errorf("cannot use immutable account role for account creation")
	}

	return nil, nil
}

// New returns a new instance of the connector.
func New(ctx context.Context, apiKey string, allowInsecure bool, opts ...Opt) (*Connector, error) {
	var clientOpts []client.Opt
	if allowInsecure {
		clientOpts = append(clientOpts, client.AllowInsecure())
	}

	c, err := client.New(apiKey, clientOpts...)
	if err != nil {
		return nil, err
	}

	tc := &Connector{
		cloudServiceClient: c,
		accountCreationSettings: AccountCreationSettings{
			DefaultAccountRole: identityv1.AccountAccess_ROLE_READ,
		},
	}

	for _, opt := range opts {
		if err := opt(tc); err != nil {
			return nil, err
		}
	}

	return tc, nil
}

type Opt func(*Connector) error

// WithDefaultAccountRole sets the default account role to use for account provisioning.
// The Opt will parse the input and convert it into an identityv1.AccountAccess_Role
// using the following (case-insensitive) mapping:
//
//	AccountAccess_ROLE_UNSPECIFIED: "unspecified", "role_unspecified"
//	AccountAccess_ROLE_OWNER: "owner", "role_owner"
//	AccountAccess_ROLE_ADMIN: "admin", "role_admin"
//	AccountAccess_ROLE_DEVELOPER: "developer", "role_developer"
//	AccountAccess_ROLE_FINANCE_ADMIN: "finance-admin", "finance_admin", "role_finance_admin"
//	AccountAccess_ROLE_READ: "read", "role_read"
func WithDefaultAccountRole(role string) Opt {
	return func(c *Connector) error {
		r, err := AccountAccessRoleFromString(role)
		if err != nil {
			return err
		}
		c.accountCreationSettings.DefaultAccountRole = *r
		return nil
	}
}
