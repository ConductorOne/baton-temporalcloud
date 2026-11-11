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

	// apiKeyExpiryReadbackTolerance bounds how much EARLIER than requested the
	// expiry the provider reports may be before issuance fails. The provider
	// rounds the instant it stores, so a slightly shorter lifetime is expected.
	//
	// The comparison is one-sided on purpose: a provider expiry LATER than the
	// approved deadline is rejected with no tolerance at all, because a
	// credential that outlives its approved lifetime is the direction that
	// matters. This constant never licenses a later expiry.
	apiKeyExpiryReadbackTolerance = time.Minute

	// apiKeyReadbackAttempts and apiKeyReadbackInterval bound how long the
	// post-create read-back retries a NotFound when the create returned no
	// async operation to await, so the read can race the provider's own write.
	apiKeyReadbackAttempts = 4
	apiKeyReadbackInterval = 500 * time.Millisecond

	// apiKeyCreateMaxDuration bounds the wait for the provider's asynchronous
	// create of an issued key.
	apiKeyCreateMaxDuration = 2 * time.Minute

	// apiKeyDeletionMaxDuration bounds the wait for the provider's
	// asynchronous delete of a key.
	apiKeyDeletionMaxDuration = 10 * time.Minute

	// apiKeyAsyncCheckFallback is used when the provider does not populate an
	// async operation's check duration, so the wait loop cannot busy-spin.
	apiKeyAsyncCheckFallback = 2 * time.Second
)

var _ connectorbuilder.ResourceSyncerV2 = (*apiKeyBuilder)(nil)
var _ connectorbuilder.ResourceDeleterV2Limited = (*apiKeyBuilder)(nil)
var _ connectorbuilder.ResourceActionProvider = (*apiKeyBuilder)(nil)

// apiKeyBuilder syncs one owner kind's Temporal Cloud API keys as secret
// resources and provides the provider revoke path for them. There is one
// builder per owner kind (see apiKeyKind), so each secret resource type has
// exactly one kind of owning identity.
//
// Only the service-account builder registers an issuer; both revoke through the
// same Delete, because the key id is the provider's handle for either owner.
type apiKeyBuilder struct {
	client cloudservicev1.CloudServiceClient
	kind   apiKeyKind
}

func newServiceAccountAPIKeyBuilder(client cloudservicev1.CloudServiceClient) *apiKeyBuilder {
	return &apiKeyBuilder{client: client, kind: serviceAccountAPIKeyKind}
}

func newUserAPIKeyBuilder(client cloudservicev1.CloudServiceClient) *apiKeyBuilder {
	return &apiKeyBuilder{client: client, kind: userAPIKeyKind}
}

func (o *apiKeyBuilder) ResourceType(_ context.Context) *v2.ResourceType {
	return o.kind.resourceType
}

// List returns Temporal Cloud API keys as secret resources. It reads the
// account-wide key list rather than filtering to one owner: the connector
// authenticates as an account-wide principal, and a filtered walk would report
// a successful sync that omits keys C1 then reads as deleted.
//
// Each builder keeps only the keys its owner kind owns: the two secret resource
// types together cover every key the provider reports, and neither reports the
// other's. A key with no recognised owner kind belongs to neither type.
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
			ResourceTypeID: o.kind.resourceType.Id,
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
		if apiKeyIsTerminal(key.GetState()) || key.GetSpec().GetOwnerType() != o.kind.ownerType {
			continue
		}
		keyResource, err := protoAPIKeyToResource(key, o.kind)
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
	if apiKeyIsAlreadyGone(existing.GetApiKey().GetState()) {
		// The provider already retired this key. Issuing the delete anyway asks
		// it to delete something it has already deleted, and a provider that
		// refuses that turns a revoke of an already-gone key into a failure --
		// the opposite of the idempotency this path documents.
		return nil, nil
	}
	if existing.GetApiKey().GetState() == resourcev1.ResourceState_RESOURCE_STATE_DELETING {
		// The provider is already deleting this key, so a second delete would
		// ask it to delete something it is in the middle of deleting. It is not
		// gone either: a key still being torn down can still authenticate, and
		// reporting success here would tell C1 the revoke had completed while
		// the credential was still usable. Wait for the state the provider
		// settles on, the same guarantee a fresh delete gives.
		return nil, o.awaitAPIKeyRetired(ctx, keyID)
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

// awaitAPIKeyRetired waits for a key the provider is already deleting to reach
// a state where it can no longer authenticate.
//
// There is no asynchronous operation id to await here -- the delete was issued
// by an earlier caller -- so the key's own state is the only signal. The wait is
// bounded by apiKeyDeletionMaxDuration, the same bound a fresh delete's
// asynchronous operation gets, so a provider that never settles fails the revoke
// instead of hanging it. A state other than DELETING (for example DELETE_FAILED)
// fails the revoke at once. NotFound is success: a key the provider no longer lists
// is retired for every purpose a revoke cares about.
func (o *apiKeyBuilder) awaitAPIKeyRetired(ctx context.Context, keyID string) error {
	waitCtx, cancel := context.WithTimeout(ctx, apiKeyDeletionMaxDuration)
	defer cancel()

	for {
		key, err := o.client.GetApiKey(waitCtx, &cloudservicev1.GetApiKeyRequest{KeyId: keyID})
		if err != nil {
			if status.Code(err) == codes.NotFound {
				return nil
			}
			return fmt.Errorf("baton-temporalcloud: failed to read API key %q while waiting for its delete: %w", keyID, err)
		}
		state := key.GetApiKey().GetState()
		if apiKeyIsAlreadyGone(state) {
			return nil
		}
		if state != resourcev1.ResourceState_RESOURCE_STATE_DELETING {
			// The earlier delete did not finish: the provider reports the key
			// as DELETE_FAILED, or has returned it to a live state. It can
			// still authenticate, so this revoke has not happened. Polling
			// would only delay the same answer by apiKeyDeletionMaxDuration.
			return fmt.Errorf("baton-temporalcloud: API key %q is %s, not deleting: the provider's earlier delete did not complete, retry the revoke", keyID, state)
		}

		select {
		case <-waitCtx.Done():
			return fmt.Errorf("baton-temporalcloud: API key %q is still %s after %s: %w",
				keyID, key.GetApiKey().GetState(), apiKeyDeletionMaxDuration, waitCtx.Err())
		case <-time.After(apiKeyAsyncCheckFallback):
		}
	}
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
func protoAPIKeyToResource(key *identityv1.ApiKey, kind apiKeyKind) (*v2.Resource, error) {
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

	if owner := apiKeyOwnerResourceID(key.GetSpec(), kind); owner != nil {
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

	return rs.NewSecretResource(name, kind.resourceType, key.GetId(), traitOpts, resourceOpts...)
}

// apiKeyOwnerResourceID maps a key's owner onto the connector resource that
// represents it. An owner of any other kind yields nil rather than a guess: a
// credential attributed to the wrong identity is worse than one with no
// identity stamped on it.
func apiKeyOwnerResourceID(spec *identityv1.ApiKeySpec, kind apiKeyKind) *v2.ResourceId {
	if spec.GetOwnerId() == "" || spec.GetOwnerType() != kind.ownerType {
		return nil
	}
	return &v2.ResourceId{ResourceType: kind.ownerResourceType.Id, Resource: spec.GetOwnerId()}
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

// apiKeyIsAlreadyGone reports whether a key the provider still lists can no
// longer authenticate and needs no delete issued for it: deleted or expired.
// Distinct from apiKeyIsTerminal, which decides what a sync shows.
//
// DELETING is deliberately NOT included. A key the provider is still tearing
// down can still authenticate, so treating it as gone would let a revoke report
// success while the credential was still usable. Delete waits it out instead.
func apiKeyIsAlreadyGone(state resourcev1.ResourceState) bool {
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
