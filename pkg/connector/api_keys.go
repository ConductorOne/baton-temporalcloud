package connector

import (
	"context"
	"fmt"
	"time"

	"github.com/grpc-ecosystem/go-grpc-middleware/logging/zap/ctxzap"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	"github.com/conductorone/baton-sdk/pkg/annotations"
	"github.com/conductorone/baton-sdk/pkg/connectorbuilder"
	"github.com/conductorone/baton-sdk/pkg/pagination"
	rs "github.com/conductorone/baton-sdk/pkg/types/resource"

	cloudservicev1 "go.temporal.io/cloud-sdk/api/cloudservice/v1"
	identityv1 "go.temporal.io/cloud-sdk/api/identity/v1"
	resourcev1 "go.temporal.io/cloud-sdk/api/resource/v1"
)

const (
	// apiKeyCredentialDetail refines SecretTrait.credential_type
	// (STATIC_SECRET) into the provider-specific kind. It is a structured
	// field on the trait, so a reader never has to infer the credential kind
	// from display-name text.
	apiKeyCredentialDetail = "temporal_cloud.api_key"

	// apiKeyNamePrefix makes a vended key's provider-side display name a
	// deterministic function of the C1 request id. Temporal Cloud has no
	// create-if-absent, so the name is how a retried request recognises the
	// key its predecessor created instead of minting a second one.
	apiKeyNamePrefix = "c1-"

	// apiKeyDefaultTTL is the expiry applied when a request carries none.
	// Temporal Cloud API keys expire, and the connector does not mint
	// never-expiring credentials, so an absent caller-selected expiry still
	// produces a bounded key. 90 days is a deliberately conservative default
	// well inside the provider maximum.
	apiKeyDefaultTTL = 90 * 24 * time.Hour

	// apiKeyMinTTL bounds a caller-selected expiry from below, so an
	// already-expired or absurdly short key cannot be requested.
	apiKeyMinTTL = 1 * time.Minute

	// apiKeyMaxTTL is the provider's documented maximum API-key expiry.
	// Verified 2026-10-06 against
	// https://docs.temporal.io/cloud/api-keys ("The maximum expiration time
	// for an API key is 2 years"). The issue that requested this connector
	// asserted a 90-day provider maximum; the official documentation says
	// otherwise, so the enforced ceiling follows the documentation and the
	// 90-day figure survives only as apiKeyDefaultTTL.
	apiKeyMaxTTL = 2 * 365 * 24 * time.Hour

	// apiKeyDeletionMaxDuration bounds the wait for the provider's
	// asynchronous delete of a vended key.
	apiKeyDeletionMaxDuration = 10 * time.Minute

	// apiKeyAsyncCheckFallback is used when the provider does not populate an
	// async operation's check duration, so the wait loop cannot busy-spin.
	apiKeyAsyncCheckFallback = 2 * time.Second

	// apiKeyMaxListPages bounds the duplicate-issuance lookup so a provider
	// that ignores page tokens fails closed instead of paging forever. A
	// single service account may hold at most 20 non-expired keys.
	apiKeyMaxListPages = 100
)

var _ connectorbuilder.ResourceSyncerV2 = (*apiKeyBuilder)(nil)
var _ connectorbuilder.ResourceDeleterV2Limited = (*apiKeyBuilder)(nil)

// apiKeyBuilder syncs Temporal Cloud API keys as secret resources and provides
// the provider revoke path for credentials this connector mints.
//
// The resource type is what makes credential issuance possible at all: the SDK
// refuses to advertise an issuance descriptor whose secret resource type has no
// ResourceDeleterV2, and C1 drops a descriptor whose secret resource type the
// connector does not also declare as a synced resource type.
type apiKeyBuilder struct {
	client cloudservicev1.CloudServiceClient
}

func newAPIKeyBuilder(client cloudservicev1.CloudServiceClient) *apiKeyBuilder {
	return &apiKeyBuilder{client: client}
}

func (o *apiKeyBuilder) ResourceType(_ context.Context) *v2.ResourceType {
	return apiKeyResourceType
}

// List returns Temporal Cloud API keys as secret resources. It reads the
// account-wide key list rather than filtering to one owner: the connector
// authenticates as an account-wide principal, and a filtered walk would report
// a successful sync that omits keys C1 then reads as deleted.
//
// Deleted and expired keys are skipped. They cannot authenticate, and a
// terminal-state key surfaced as a live resource would keep a dead credential
// in the inventory forever.
func (o *apiKeyBuilder) List(ctx context.Context, parentResourceID *v2.ResourceId, opts rs.SyncOpAttrs) ([]*v2.Resource, *rs.SyncOpResults, error) {
	bag := &pagination.Bag{}
	if err := bag.Unmarshal(opts.PageToken.Token); err != nil {
		return nil, nil, err
	}

	if bag.Current() == nil {
		bag.Push(pagination.PageState{
			ResourceTypeID: apiKeyResourceType.Id,
		})
	}

	req := &cloudservicev1.GetApiKeysRequest{}
	if bag.PageToken() != "" {
		req.PageToken = bag.PageToken()
	}

	resp, err := o.client.GetApiKeys(ctx, req)
	if err != nil {
		return nil, nil, fmt.Errorf("baton-temporalcloud: failed to list API keys: %w", err)
	}

	rv := make([]*v2.Resource, 0, len(resp.GetApiKeys()))
	for _, key := range resp.GetApiKeys() {
		if apiKeyIsTerminal(key.GetState()) {
			continue
		}
		keyResource, err := protoAPIKeyToResource(key)
		if err != nil {
			return nil, nil, err
		}
		rv = append(rv, keyResource)
	}

	return paginate(rv, bag, resp.GetNextPageToken())
}

// Entitlements always returns an empty slice for API keys.
func (o *apiKeyBuilder) Entitlements(_ context.Context, _ *v2.Resource, _ rs.SyncOpAttrs) ([]*v2.Entitlement, *rs.SyncOpResults, error) {
	return nil, nil, nil
}

// Grants always returns an empty slice for API keys.
func (o *apiKeyBuilder) Grants(_ context.Context, _ *v2.Resource, _ rs.SyncOpAttrs) ([]*v2.Grant, *rs.SyncOpResults, error) {
	return nil, nil, nil
}

// Delete revokes one API key. It deletes the key alone: the owning service
// account, its namespace assignments and its other keys are untouched.
//
// Already-gone is a success, not a failure. The key is read first so the
// provider's own answer decides that case: a NotFound on the read means the
// handle names nothing at the provider, which is exactly the idempotent
// outcome a delete of an already-deleted key should report. The read also
// supplies the key's current resource version, so a delete is never rejected
// for carrying a version a previous update superseded.
//
// Every other error is propagated with its gRPC code intact, so a retryable
// provider failure stays retryable and a genuinely terminal one stays terminal.
func (o *apiKeyBuilder) Delete(ctx context.Context, resourceID *v2.ResourceId, _ *v2.ResourceId) (annotations.Annotations, error) {
	if resourceID == nil || resourceID.GetResource() == "" {
		return nil, status.Error(codes.InvalidArgument, "baton-temporalcloud: API key id is required")
	}
	keyID := resourceID.GetResource()

	existing, err := o.client.GetApiKey(ctx, &cloudservicev1.GetApiKeyRequest{KeyId: keyID})
	if err != nil {
		if status.Code(err) == codes.NotFound {
			// The provider has no such key. That is the state a successful
			// delete would have produced, so report success.
			return nil, nil
		}
		return nil, fmt.Errorf("baton-temporalcloud: failed to read API key %q before delete: %w", keyID, err)
	}

	resp, err := o.client.DeleteApiKey(ctx, &cloudservicev1.DeleteApiKeyRequest{
		KeyId:           keyID,
		ResourceVersion: existing.GetApiKey().GetResourceVersion(),
	})
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return nil, nil
		}
		return nil, fmt.Errorf("baton-temporalcloud: failed to delete API key %q: %w", keyID, err)
	}

	if opID := resp.GetAsyncOperation().GetId(); opID != "" {
		l := ctxzap.Extract(ctx).With(
			zap.String("request_id", opID),
			zap.String("api_key_id", keyID),
		)
		retryDelay := asyncCheckDelay(resp.GetAsyncOperation().GetCheckDuration().AsDuration())
		waitCtx, cancel := context.WithTimeout(ctx, apiKeyDeletionMaxDuration)
		defer cancel()
		if err := awaitAsyncOperation(waitCtx, l, o.client, opID, retryDelay); err != nil {
			return nil, fmt.Errorf("baton-temporalcloud: API key %q deletion failed: %w", keyID, err)
		}
	}

	return nil, nil
}

// protoAPIKeyToResource builds the synced resource for one Temporal Cloud API
// key. The resource id is the bare provider key id, which is the same handle
// Delete consumes, so a synced key and an issued key are revocable through one
// path.
//
// CreatedById is deliberately left unset: Temporal Cloud's API-key API never
// reports who created a key, and this connector authenticates as its own
// configured principal rather than as the recipient, so recording the owner as
// creator would assert a fact the provider never reported.
func protoAPIKeyToResource(key *identityv1.ApiKey) (*v2.Resource, error) {
	if key.GetId() == "" {
		return nil, fmt.Errorf("baton-temporalcloud: API key is missing an id")
	}

	name := key.GetSpec().GetDisplayName()
	if name == "" {
		name = key.GetId()
	}

	traitOpts := []rs.SecretTraitOption{
		rs.WithSecretType(v2.SecretTrait_CREDENTIAL_TYPE_STATIC_SECRET),
		rs.WithSecretDetail(apiKeyCredentialDetail),
	}
	var resourceOpts []rs.ResourceOption

	if owner := apiKeyOwnerResourceID(key.GetSpec()); owner != nil {
		traitOpts = append(traitOpts, rs.WithSecretIdentityID(owner))
		// The owner is the resource's parent so the delete path can see the
		// key's provider hierarchy, matching how the issued resource is built.
		resourceOpts = append(resourceOpts, rs.WithParentResourceID(owner))
	}
	if expiry := key.GetSpec().GetExpiryTime(); expiry != nil {
		traitOpts = append(traitOpts, rs.WithSecretExpiresAt(expiry.AsTime()))
	}
	if created := key.GetCreatedTime(); created != nil {
		resourceOpts = append(resourceOpts, rs.WithResourceCreatedAt(created.AsTime()))
	}

	return rs.NewSecretResource(name, apiKeyResourceType, key.GetId(), traitOpts, resourceOpts...)
}

// apiKeyOwnerResourceID maps a key's owner onto the connector resource that
// represents it. An unrecognised or absent owner yields nil rather than a
// guess: a credential attributed to the wrong identity is worse than one with
// no identity stamped on it.
func apiKeyOwnerResourceID(spec *identityv1.ApiKeySpec) *v2.ResourceId {
	if spec.GetOwnerId() == "" {
		return nil
	}
	switch spec.GetOwnerType() {
	case identityv1.OwnerType_OWNER_TYPE_SERVICE_ACCOUNT:
		return &v2.ResourceId{ResourceType: serviceAccountResourceType.Id, Resource: spec.GetOwnerId()}
	case identityv1.OwnerType_OWNER_TYPE_USER:
		return &v2.ResourceId{ResourceType: userResourceType.Id, Resource: spec.GetOwnerId()}
	default:
		return nil
	}
}

// apiKeyIsTerminal reports whether a provider key state means the key can no
// longer authenticate. Only the two terminal states are treated that way; a
// transient or error state stays visible, because hiding a key C1 cannot see
// is a worse failure than showing one mid-transition.
func apiKeyIsTerminal(state resourcev1.ResourceState) bool {
	switch state {
	case resourcev1.ResourceState_RESOURCE_STATE_DELETED,
		resourcev1.ResourceState_RESOURCE_STATE_EXPIRED:
		return true
	default:
		return false
	}
}

// asyncCheckDelay keeps the async wait loop from busy-spinning when the
// provider reports no check duration.
func asyncCheckDelay(d time.Duration) time.Duration {
	if d <= 0 {
		return apiKeyAsyncCheckFallback
	}
	return d
}
