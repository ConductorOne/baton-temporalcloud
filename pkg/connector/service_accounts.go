package connector

import (
	"context"
	"fmt"
	"time"

	"github.com/grpc-ecosystem/go-grpc-middleware/logging/zap/ctxzap"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	"github.com/conductorone/baton-sdk/pkg/annotations"
	"github.com/conductorone/baton-sdk/pkg/connectorbuilder"
	"github.com/conductorone/baton-sdk/pkg/pagination"
	rs "github.com/conductorone/baton-sdk/pkg/types/resource"

	cloudservicev1 "go.temporal.io/cloud-sdk/api/cloudservice/v1"
	identityv1 "go.temporal.io/cloud-sdk/api/identity/v1"
)

var _ connectorbuilder.ResourceSyncerV2 = (*serviceAccountBuilder)(nil)
var _ connectorbuilder.CredentialIssuerV2 = (*serviceAccountBuilder)(nil)

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

// IssueCapabilityDetails advertises the credential kinds this connector mints.
//
// There is exactly one: an API key owned by the service account the request
// names. It is DISCOVERABLE because Temporal Cloud can enumerate API keys, and
// the api-key resource type above is declared and synced so an issued key
// appears in subsequent syncs instead of only in the issuance response.
//
// The advertised expiry window is the provider's, not a product default: the
// connector accepts any caller-selected expiry from one minute up to Temporal
// Cloud's documented two-year maximum, and applies its own 90-day default only
// when a request carries none.
func (o *serviceAccountBuilder) IssueCapabilityDetails(_ context.Context) (*v2.CredentialDetailsCredentialIssue, annotations.Annotations, error) {
	return v2.CredentialDetailsCredentialIssue_builder{
		Options: []*v2.CredentialIssueOptionDescriptor{
			v2.CredentialIssueOptionDescriptor_builder{
				Option:               v2.CapabilityDetailCredentialOption_CAPABILITY_DETAIL_CREDENTIAL_OPTION_API_KEY,
				ResourceMode:         v2.CredentialResourceMode_CREDENTIAL_RESOURCE_MODE_DISCOVERABLE,
				SecretResourceTypeId: apiKeyResourceType.Id,
				Expiry: v2.IssuanceExpiryCapability_builder{
					Min: durationpb.New(apiKeyMinTTL),
					Max: durationpb.New(apiKeyMaxTTL),
				}.Build(),
			}.Build(),
		},
		PreferredOption: v2.CapabilityDetailCredentialOption_CAPABILITY_DETAIL_CREDENTIAL_OPTION_API_KEY,
	}.Build(), nil, nil
}

// Issue mints one Temporal Cloud API key owned by the requested service
// account. It never creates or modifies an identity: the owner must already
// exist and be synced, and the key inherits the owner's permissions.
//
// Idempotency is by provider-side name. The key's display name is derived from
// the C1 request id, and an existing key with that name is refused rather than
// duplicated. Temporal Cloud returns a key's secret exactly once, at creation,
// so an existing key cannot be handed back: failing closed is the only
// duplicate-free answer, and it keeps a retried request at exactly one
// provider credential.
//
// The create is deliberately not awaited to completion. The response already
// carries the key id and secret, and a create whose wait timed out would leave
// a key this call can never return -- an orphan the retry would then refuse as
// a duplicate. Returning the provider's own answer keeps "created" and
// "returned" the same event.
func (o *serviceAccountBuilder) Issue(ctx context.Context, input *connectorbuilder.CredentialIssueInput) (*connectorbuilder.CredentialIssueOutput, error) {
	if input == nil || input.IdentityID == nil || input.IdentityID.GetResourceType() != serviceAccountResourceType.Id {
		return nil, status.Error(codes.InvalidArgument, "baton-temporalcloud: a Temporal Cloud service account identity is required")
	}
	if got := input.CredentialOptions.GetSecretResourceTypeId(); got != apiKeyResourceType.Id {
		return nil, status.Errorf(codes.InvalidArgument, "baton-temporalcloud: unsupported credential secret resource type %q", got)
	}
	ownerID := input.IdentityID.GetResource()
	if ownerID == "" {
		return nil, status.Error(codes.InvalidArgument, "baton-temporalcloud: service account id is required")
	}

	now := time.Now()
	expiresAt, err := apiKeyExpiry(input.ExpiresAt, now)
	if err != nil {
		return nil, err
	}

	name := issuedCredentialName(input.RequestID)
	existing, err := o.findAPIKeyByName(ctx, ownerID, name)
	if err != nil {
		return nil, err
	}
	if existing != "" {
		return nil, status.Errorf(codes.AlreadyExists,
			"baton-temporalcloud: an API key for request %q already exists for service account %q (key %s); refusing to issue a duplicate",
			input.RequestID, ownerID, existing)
	}

	resp, err := o.client.CreateApiKey(ctx, &cloudservicev1.CreateApiKeyRequest{
		Spec: &identityv1.ApiKeySpec{
			OwnerId:     ownerID,
			OwnerType:   identityv1.OwnerType_OWNER_TYPE_SERVICE_ACCOUNT,
			DisplayName: name,
			Description: fmt.Sprintf("ConductorOne vended credential for request %s", input.RequestID),
			ExpiryTime:  timestamppb.New(expiresAt),
		},
	})
	if err != nil {
		return nil, apiKeyCreateError(err)
	}
	keyID := resp.GetKeyId()
	if keyID == "" {
		return nil, status.Error(codes.Internal, "baton-temporalcloud: provider returned an API key with no id")
	}

	secret, err := rs.NewSecretResource(name, apiKeyResourceType, keyID, []rs.SecretTraitOption{
		// The SDK requires the secret trait's identity to equal the
		// authenticating identity, so this is the requested service account,
		// not whoever happens to be the connector's principal.
		rs.WithSecretIdentityID(input.IdentityID),
		rs.WithSecretType(v2.SecretTrait_CREDENTIAL_TYPE_STATIC_SECRET),
		rs.WithSecretDetail(apiKeyCredentialDetail),
		rs.WithSecretExpiresAt(expiresAt),
	}, rs.WithParentResourceID(input.IdentityID), rs.WithResourceCreatedAt(now))
	if err != nil {
		// The key exists at the provider but cannot be returned. Leaving it
		// would consume one of the service account's limited key slots with a
		// credential nobody can use, so the mint is rolled back.
		if _, deleteErr := o.client.DeleteApiKey(ctx, &cloudservicev1.DeleteApiKeyRequest{KeyId: keyID}); deleteErr != nil {
			ctxzap.Extract(ctx).Warn(
				"baton-temporalcloud: failed to clean up API key after resource construction error",
				zap.String("api_key_id", keyID),
				zap.String("service_account_id", ownerID),
				zap.Error(deleteErr),
			)
		}
		return nil, fmt.Errorf("baton-temporalcloud: build API key secret resource: %w", err)
	}

	return &connectorbuilder.CredentialIssueOutput{
		Secret: secret,
		PlaintextData: []*v2.PlaintextData{
			v2.PlaintextData_builder{Name: "api_key", Bytes: []byte(resp.GetToken())}.Build(),
		},
		ResourceMode: v2.CredentialResourceMode_CREDENTIAL_RESOURCE_MODE_DISCOVERABLE,
	}, nil
}

// issuedCredentialName is the provider-side display name this connector gives
// a credential it mints. Deriving it from the request id is what makes a
// retried request find its predecessor's key.
func issuedCredentialName(requestID string) string {
	return apiKeyNamePrefix + requestID
}

// apiKeyExpiry resolves the expiry to hand the provider. A request that names
// no expiry still gets a bounded key: Temporal Cloud API keys expire, and a
// never-expiring vended credential is not a thing this connector will mint.
//
// The bounds are re-checked here, not only advertised, because a direct Issue
// call bypasses the SDK's request-time validation, and because the provider's
// maximum has to hold even for a caller that never declares an expiry.
func apiKeyExpiry(requested *timestamppb.Timestamp, now time.Time) (time.Time, error) {
	if requested == nil {
		return now.Add(apiKeyDefaultTTL), nil
	}
	if err := requested.CheckValid(); err != nil {
		return time.Time{}, status.Errorf(codes.InvalidArgument, "baton-temporalcloud: requested expiry is invalid: %v", err)
	}
	remaining := requested.AsTime().Sub(now)
	if remaining <= 0 {
		return time.Time{}, status.Error(codes.InvalidArgument, "baton-temporalcloud: requested expiry is not in the future")
	}
	if remaining < apiKeyMinTTL {
		return time.Time{}, status.Errorf(codes.InvalidArgument,
			"baton-temporalcloud: requested expiry %s is below the connector minimum of %s", remaining, apiKeyMinTTL)
	}
	if remaining > apiKeyMaxTTL {
		return time.Time{}, status.Errorf(codes.InvalidArgument,
			"baton-temporalcloud: requested expiry %s exceeds the Temporal Cloud maximum of %s", remaining, apiKeyMaxTTL)
	}
	return requested.AsTime(), nil
}

// apiKeyCreateError surfaces the provider's own failure, with a hint for the
// limit a service account is most likely to hit. The error is wrapped rather
// than replaced so its gRPC code survives.
func apiKeyCreateError(err error) error {
	switch status.Code(err) {
	case codes.ResourceExhausted:
		return fmt.Errorf("baton-temporalcloud: failed to create service account API key: %w "+
			"(Temporal Cloud allows at most 20 non-expired API keys per service account; revoke one and retry)", err)
	case codes.FailedPrecondition:
		return fmt.Errorf("baton-temporalcloud: failed to create service account API key: %w "+
			"(the service account may be at Temporal Cloud's limit of 20 non-expired API keys, or API-key creation may be disabled for the account)", err)
	default:
		return fmt.Errorf("baton-temporalcloud: failed to create service account API key: %w", err)
	}
}

// findAPIKeyByName returns the id of the named key owned by ownerID, or "" when
// none exists. Temporal Cloud has no lookup-by-name, so this walks the owner's
// keys; a service account holds at most 20 non-expired keys, so one page
// normally settles it.
func (o *serviceAccountBuilder) findAPIKeyByName(ctx context.Context, ownerID, name string) (string, error) {
	var pageToken string
	for range apiKeyMaxListPages {
		resp, err := o.client.GetApiKeys(ctx, &cloudservicev1.GetApiKeysRequest{
			OwnerId:   ownerID,
			OwnerType: identityv1.OwnerType_OWNER_TYPE_SERVICE_ACCOUNT,
			PageToken: pageToken,
		})
		if err != nil {
			return "", fmt.Errorf("baton-temporalcloud: failed to look up existing API keys for service account %q: %w", ownerID, err)
		}
		for _, key := range resp.GetApiKeys() {
			if key.GetSpec().GetDisplayName() == name {
				return key.GetId(), nil
			}
		}
		pageToken = resp.GetNextPageToken()
		if pageToken == "" {
			return "", nil
		}
	}
	return "", fmt.Errorf("baton-temporalcloud: exceeded %d pages looking up API key %q for service account %q",
		apiKeyMaxListPages, name, ownerID)
}
