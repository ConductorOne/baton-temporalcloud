package connector

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	"github.com/conductorone/baton-sdk/pkg/annotations"
	rs "github.com/conductorone/baton-sdk/pkg/types/resource"

	cloudservicev1 "go.temporal.io/cloud-sdk/api/cloudservice/v1"
	identityv1 "go.temporal.io/cloud-sdk/api/identity/v1"
	operationv1 "go.temporal.io/cloud-sdk/api/operation/v1"
	resourcev1 "go.temporal.io/cloud-sdk/api/resource/v1"
)

// fakeCloudService implements the CloudService RPCs the API-key paths call.
// The embedded nil interface satisfies the rest of CloudServiceClient, so an
// unexpected RPC panics instead of silently returning a zero value.
type fakeCloudService struct {
	cloudservicev1.CloudServiceClient

	createApiKey func(context.Context, *cloudservicev1.CreateApiKeyRequest) (*cloudservicev1.CreateApiKeyResponse, error)
	getApiKey    func(context.Context, *cloudservicev1.GetApiKeyRequest) (*cloudservicev1.GetApiKeyResponse, error)
	deleteApiKey func(context.Context, *cloudservicev1.DeleteApiKeyRequest) (*cloudservicev1.DeleteApiKeyResponse, error)
	getApiKeys   func(context.Context, *cloudservicev1.GetApiKeysRequest) (*cloudservicev1.GetApiKeysResponse, error)
	getAsyncOp   func(context.Context, *cloudservicev1.GetAsyncOperationRequest) (*cloudservicev1.GetAsyncOperationResponse, error)
}

func (f *fakeCloudService) CreateApiKey(ctx context.Context, in *cloudservicev1.CreateApiKeyRequest, _ ...grpc.CallOption) (*cloudservicev1.CreateApiKeyResponse, error) {
	return f.createApiKey(ctx, in)
}

func (f *fakeCloudService) GetApiKey(ctx context.Context, in *cloudservicev1.GetApiKeyRequest, _ ...grpc.CallOption) (*cloudservicev1.GetApiKeyResponse, error) {
	return f.getApiKey(ctx, in)
}

func (f *fakeCloudService) DeleteApiKey(ctx context.Context, in *cloudservicev1.DeleteApiKeyRequest, _ ...grpc.CallOption) (*cloudservicev1.DeleteApiKeyResponse, error) {
	return f.deleteApiKey(ctx, in)
}

func (f *fakeCloudService) GetApiKeys(ctx context.Context, in *cloudservicev1.GetApiKeysRequest, _ ...grpc.CallOption) (*cloudservicev1.GetApiKeysResponse, error) {
	return f.getApiKeys(ctx, in)
}

func (f *fakeCloudService) GetAsyncOperation(ctx context.Context, in *cloudservicev1.GetAsyncOperationRequest, _ ...grpc.CallOption) (*cloudservicev1.GetAsyncOperationResponse, error) {
	return f.getAsyncOp(ctx, in)
}

// capabilityProvider is the SDK builder method that computes the connector's
// advertised capabilities. connectorbuilder exposes it on the concrete builder

// TestDeleteAPIKeyRevokesExactlyOneKey pins the revoke contract: the key is
// read for its current resource version and deleted by id, and the provider's
// asynchronous delete is awaited before the delete reports success.
func TestDeleteAPIKeyRevokesExactlyOneKey(t *testing.T) {
	t.Parallel()

	var deleted *cloudservicev1.DeleteApiKeyRequest
	fake := &fakeCloudService{
		getApiKey: func(_ context.Context, in *cloudservicev1.GetApiKeyRequest) (*cloudservicev1.GetApiKeyResponse, error) {
			require.Equal(t, "key-1", in.GetKeyId())
			return &cloudservicev1.GetApiKeyResponse{ApiKey: &identityv1.ApiKey{Id: "key-1", ResourceVersion: "v7"}}, nil
		},
		deleteApiKey: func(_ context.Context, in *cloudservicev1.DeleteApiKeyRequest) (*cloudservicev1.DeleteApiKeyResponse, error) {
			deleted = in
			return &cloudservicev1.DeleteApiKeyResponse{AsyncOperation: &operationv1.AsyncOperation{Id: "op-1"}}, nil
		},
		getAsyncOp: func(_ context.Context, in *cloudservicev1.GetAsyncOperationRequest) (*cloudservicev1.GetAsyncOperationResponse, error) {
			require.Equal(t, "op-1", in.GetAsyncOperationId())
			return &cloudservicev1.GetAsyncOperationResponse{AsyncOperation: &operationv1.AsyncOperation{
				Id:    "op-1",
				State: operationv1.AsyncOperation_STATE_FULFILLED,
			}}, nil
		},
	}

	_, err := newServiceAccountAPIKeyBuilder(fake).Delete(context.Background(), &v2.ResourceId{ResourceType: serviceAccountAPIKeyResourceType.Id, Resource: "key-1"}, nil)
	require.NoError(t, err)
	require.NotNil(t, deleted)
	require.Equal(t, "key-1", deleted.GetKeyId())
	require.Equal(t, "v7", deleted.GetResourceVersion())
}

// TestDeleteAPIKeyTreatsAlreadyGoneAsSuccess is the idempotent-deletion
// contract: a handle the provider no longer knows is a completed delete, not
// an error, and no delete RPC is issued for it.
func TestDeleteAPIKeyTreatsAlreadyGoneAsSuccess(t *testing.T) {
	t.Parallel()

	fake := &fakeCloudService{
		getApiKey: func(context.Context, *cloudservicev1.GetApiKeyRequest) (*cloudservicev1.GetApiKeyResponse, error) {
			return nil, status.Error(codes.NotFound, "api key not found")
		},
		deleteApiKey: func(context.Context, *cloudservicev1.DeleteApiKeyRequest) (*cloudservicev1.DeleteApiKeyResponse, error) {
			t.Fatal("no delete may be issued for a key the provider no longer knows")
			return nil, nil
		},
	}

	_, err := newServiceAccountAPIKeyBuilder(fake).Delete(context.Background(), &v2.ResourceId{ResourceType: serviceAccountAPIKeyResourceType.Id, Resource: "key-gone"}, nil)
	require.NoError(t, err)
}

// TestDeleteAPIKeySkipsAKeyTheProviderAlreadyRetired keeps a revoke of an
// already-gone key idempotent without asking the provider to delete something it
// has already deleted: a provider that refuses that would turn a no-op revoke
// into a failure, contradicting the documented idempotency.
//
// DELETING is not in this table. A key the provider is still deleting is not
// gone -- it can still authenticate -- so Delete waits it out rather than
// reporting success; that behaviour is covered below.
func TestDeleteAPIKeySkipsAKeyTheProviderAlreadyRetired(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		state resourcev1.ResourceState
	}{
		{name: "deleted", state: resourcev1.ResourceState_RESOURCE_STATE_DELETED},
		{name: "expired", state: resourcev1.ResourceState_RESOURCE_STATE_EXPIRED},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			fake := &fakeCloudService{
				getApiKey: func(context.Context, *cloudservicev1.GetApiKeyRequest) (*cloudservicev1.GetApiKeyResponse, error) {
					return &cloudservicev1.GetApiKeyResponse{ApiKey: &identityv1.ApiKey{
						Id: "key-1", State: tc.state, ResourceVersion: "v1",
					}}, nil
				},
				deleteApiKey: func(context.Context, *cloudservicev1.DeleteApiKeyRequest) (*cloudservicev1.DeleteApiKeyResponse, error) {
					t.Fatal("no delete may be issued for a key the provider has already retired")
					return nil, nil
				},
			}

			_, err := newServiceAccountAPIKeyBuilder(fake).Delete(context.Background(), &v2.ResourceId{ResourceType: serviceAccountAPIKeyResourceType.Id, Resource: "key-1"}, nil)
			require.NoError(t, err)
		})
	}
}

// TestDeleteAPIKeyWaitsOutAKeyTheProviderIsDeleting proves a revoke of a key
// the provider is mid-delete does not report success until the key can no
// longer authenticate, and never issues a second delete for it.
func TestDeleteAPIKeyWaitsOutAKeyTheProviderIsDeleting(t *testing.T) {
	t.Parallel()

	reads := 0
	fake := &fakeCloudService{
		getApiKey: func(context.Context, *cloudservicev1.GetApiKeyRequest) (*cloudservicev1.GetApiKeyResponse, error) {
			reads++
			state := resourcev1.ResourceState_RESOURCE_STATE_DELETING
			if reads > 1 {
				state = resourcev1.ResourceState_RESOURCE_STATE_DELETED
			}
			return &cloudservicev1.GetApiKeyResponse{ApiKey: &identityv1.ApiKey{
				Id: "key-1", State: state, ResourceVersion: "v1",
			}}, nil
		},
		deleteApiKey: func(context.Context, *cloudservicev1.DeleteApiKeyRequest) (*cloudservicev1.DeleteApiKeyResponse, error) {
			t.Fatal("a key the provider is already deleting must not be deleted again")
			return nil, nil
		},
	}

	_, err := newServiceAccountAPIKeyBuilder(fake).Delete(context.Background(), &v2.ResourceId{ResourceType: serviceAccountAPIKeyResourceType.Id, Resource: "key-1"}, nil)
	require.NoError(t, err)
	require.Greater(t, reads, 1, "the wait must observe the key reaching a terminal state, not assume it")
}

// TestDeleteAPIKeyTreatsAKeyVanishingWhileDeletingAsRetired proves the wait also
// ends successfully when the provider stops listing the key, which is the state
// a completed delete produces.
func TestDeleteAPIKeyTreatsAKeyVanishingWhileDeletingAsRetired(t *testing.T) {
	t.Parallel()

	reads := 0
	fake := &fakeCloudService{
		getApiKey: func(context.Context, *cloudservicev1.GetApiKeyRequest) (*cloudservicev1.GetApiKeyResponse, error) {
			reads++
			if reads > 1 {
				return nil, status.Error(codes.NotFound, "no such key")
			}
			return &cloudservicev1.GetApiKeyResponse{ApiKey: &identityv1.ApiKey{
				Id: "key-1", State: resourcev1.ResourceState_RESOURCE_STATE_DELETING, ResourceVersion: "v1",
			}}, nil
		},
		deleteApiKey: func(context.Context, *cloudservicev1.DeleteApiKeyRequest) (*cloudservicev1.DeleteApiKeyResponse, error) {
			t.Fatal("a key the provider is already deleting must not be deleted again")
			return nil, nil
		},
	}

	_, err := newServiceAccountAPIKeyBuilder(fake).Delete(context.Background(), &v2.ResourceId{ResourceType: serviceAccountAPIKeyResourceType.Id, Resource: "key-1"}, nil)
	require.NoError(t, err)
}

// TestDeleteAPIKeyFailsWhenAKeyNeverStopsDeleting proves the wait is bounded:
// a provider that never settles fails the revoke rather than reporting a
// success it cannot support, and still never issues a second delete.
//
// The bound is exercised through the caller's context, so the test does not
// have to wait out apiKeyDeletionMaxDuration to prove it exists.
func TestDeleteAPIKeyFailsWhenAKeyNeverStopsDeleting(t *testing.T) {
	t.Parallel()

	fake := &fakeCloudService{
		getApiKey: func(context.Context, *cloudservicev1.GetApiKeyRequest) (*cloudservicev1.GetApiKeyResponse, error) {
			return &cloudservicev1.GetApiKeyResponse{ApiKey: &identityv1.ApiKey{
				Id: "key-1", State: resourcev1.ResourceState_RESOURCE_STATE_DELETING, ResourceVersion: "v1",
			}}, nil
		},
		deleteApiKey: func(context.Context, *cloudservicev1.DeleteApiKeyRequest) (*cloudservicev1.DeleteApiKeyResponse, error) {
			t.Fatal("a key the provider is already deleting must not be deleted again")
			return nil, nil
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := newServiceAccountAPIKeyBuilder(fake).Delete(ctx, &v2.ResourceId{ResourceType: serviceAccountAPIKeyResourceType.Id, Resource: "key-1"}, nil)
	require.Error(t, err, "a revoke must not report success while the key can still authenticate")
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Contains(t, err.Error(), "key-1")
}

// TestDeleteAPIKeyPropagatesProviderFailure keeps a real provider failure
// distinguishable from a successful revoke.
func TestDeleteAPIKeyPropagatesProviderFailure(t *testing.T) {
	t.Parallel()

	fake := &fakeCloudService{
		getApiKey: func(context.Context, *cloudservicev1.GetApiKeyRequest) (*cloudservicev1.GetApiKeyResponse, error) {
			return &cloudservicev1.GetApiKeyResponse{ApiKey: &identityv1.ApiKey{Id: "key-1", ResourceVersion: "v1"}}, nil
		},
		deleteApiKey: func(context.Context, *cloudservicev1.DeleteApiKeyRequest) (*cloudservicev1.DeleteApiKeyResponse, error) {
			return nil, status.Error(codes.PermissionDenied, "not authorized")
		},
	}

	_, err := newServiceAccountAPIKeyBuilder(fake).Delete(context.Background(), &v2.ResourceId{ResourceType: serviceAccountAPIKeyResourceType.Id, Resource: "key-1"}, nil)
	require.Equal(t, codes.PermissionDenied, status.Code(err))
}

// TestListAPIKeysSkipsTerminalStatesAndOtherOwnerKinds proves the synced inventory carries the
// handle, owner and expiry the delete and audit paths need, and that keys the
// provider can no longer authenticate are not resurrected as live resources.
func TestListAPIKeysSkipsTerminalStatesAndOtherOwnerKinds(t *testing.T) {
	t.Parallel()

	expiry := time.Now().Add(48 * time.Hour).Truncate(time.Second)
	created := time.Now().Add(-time.Hour).Truncate(time.Second)
	fake := &fakeCloudService{
		getApiKeys: func(context.Context, *cloudservicev1.GetApiKeysRequest) (*cloudservicev1.GetApiKeysResponse, error) {
			return &cloudservicev1.GetApiKeysResponse{ApiKeys: []*identityv1.ApiKey{
				{
					Id:          "key-live",
					State:       resourcev1.ResourceState_RESOURCE_STATE_ACTIVE,
					CreatedTime: timestamppb.New(created),
					Spec: &identityv1.ApiKeySpec{
						OwnerId:     "sa-1",
						OwnerType:   identityv1.OwnerType_OWNER_TYPE_SERVICE_ACCOUNT,
						DisplayName: "vended",
						ExpiryTime:  timestamppb.New(expiry),
					},
				},
				{
					Id:    "key-user-owned",
					State: resourcev1.ResourceState_RESOURCE_STATE_ACTIVE,
					Spec:  &identityv1.ApiKeySpec{OwnerId: "u-1", OwnerType: identityv1.OwnerType_OWNER_TYPE_USER, DisplayName: "mine"},
				},
				{Id: "key-deleted", State: resourcev1.ResourceState_RESOURCE_STATE_DELETED, Spec: &identityv1.ApiKeySpec{OwnerId: "sa-1"}},
				{Id: "key-expired", State: resourcev1.ResourceState_RESOURCE_STATE_EXPIRED, Spec: &identityv1.ApiKeySpec{OwnerId: "sa-1"}},
			}}, nil
		},
	}

	resources, _, err := newServiceAccountAPIKeyBuilder(fake).List(context.Background(), nil, rs.SyncOpAttrs{})
	require.NoError(t, err)
	require.Len(t, resources, 1)

	resource := resources[0]
	require.Equal(t, "key-live", resource.GetId().GetResource())
	require.Equal(t, "vended", resource.GetDisplayName())
	require.True(t, created.Equal(resource.GetCreatedAt().AsTime()))
	require.True(t, proto.Equal(&v2.ResourceId{ResourceType: serviceAccountResourceType.Id, Resource: "sa-1"}, resource.GetParentResourceId()))

	trait := secretTrait(t, resource)
	require.True(t, proto.Equal(&v2.ResourceId{ResourceType: serviceAccountResourceType.Id, Resource: "sa-1"}, trait.GetIdentityId()))
	require.Equal(t, v2.SecretTrait_CREDENTIAL_TYPE_STATIC_SECRET, trait.GetCredentialType())
	require.True(t, expiry.Equal(trait.GetExpiresAt().AsTime()))
}

func secretTrait(t *testing.T, resource *v2.Resource) *v2.SecretTrait {
	t.Helper()
	annos := annotations.Annotations(resource.GetAnnotations())
	trait := &v2.SecretTrait{}
	found, err := annos.Pick(trait)
	require.NoError(t, err)
	require.True(t, found, "resource must carry a SecretTrait")
	return trait
}

// TestListAPIKeysSplitsByOwnerKind proves each secret resource type reports only
// its own owner kind's keys, with the owner as the matching identity type.
func TestListAPIKeysSplitsByOwnerKind(t *testing.T) {
	t.Parallel()

	fake := &fakeCloudService{
		getApiKeys: func(context.Context, *cloudservicev1.GetApiKeysRequest) (*cloudservicev1.GetApiKeysResponse, error) {
			return &cloudservicev1.GetApiKeysResponse{ApiKeys: []*identityv1.ApiKey{
				{Id: "key-sa", State: resourcev1.ResourceState_RESOURCE_STATE_ACTIVE,
					Spec: &identityv1.ApiKeySpec{OwnerId: "sa-1", OwnerType: identityv1.OwnerType_OWNER_TYPE_SERVICE_ACCOUNT}},
				{Id: "key-user", State: resourcev1.ResourceState_RESOURCE_STATE_ACTIVE,
					Spec: &identityv1.ApiKeySpec{OwnerId: "u-1", OwnerType: identityv1.OwnerType_OWNER_TYPE_USER}},
				{Id: "key-unowned", State: resourcev1.ResourceState_RESOURCE_STATE_ACTIVE, Spec: &identityv1.ApiKeySpec{}},
			}}, nil
		},
	}

	saKeys, _, err := newServiceAccountAPIKeyBuilder(fake).List(context.Background(), nil, rs.SyncOpAttrs{})
	require.NoError(t, err)
	require.Len(t, saKeys, 1)
	require.Equal(t, serviceAccountAPIKeyResourceType.Id, saKeys[0].GetId().GetResourceType())
	require.Equal(t, "key-sa", saKeys[0].GetId().GetResource())

	userKeys, _, err := newUserAPIKeyBuilder(fake).List(context.Background(), nil, rs.SyncOpAttrs{})
	require.NoError(t, err)
	require.Len(t, userKeys, 1)
	require.Equal(t, userAPIKeyResourceType.Id, userKeys[0].GetId().GetResourceType())
	require.Equal(t, "key-user", userKeys[0].GetId().GetResource())
	require.Equal(t, userResourceType.Id, secretTrait(t, userKeys[0]).GetIdentityId().GetResourceType())
}

// TestDeleteAPIKeyRetriesAKeyWhoseEarlierDeleteFailed: DELETE_FAILED is not
// gone. The key can still authenticate, so Delete issues the delete again
// instead of reporting success.
func TestDeleteAPIKeyRetriesAKeyWhoseEarlierDeleteFailed(t *testing.T) {
	t.Parallel()

	var deleted *cloudservicev1.DeleteApiKeyRequest
	fake := &fakeCloudService{
		getApiKey: func(context.Context, *cloudservicev1.GetApiKeyRequest) (*cloudservicev1.GetApiKeyResponse, error) {
			return &cloudservicev1.GetApiKeyResponse{ApiKey: &identityv1.ApiKey{
				Id: "key-1", State: resourcev1.ResourceState_RESOURCE_STATE_DELETE_FAILED, ResourceVersion: "v3",
			}}, nil
		},
		deleteApiKey: func(_ context.Context, in *cloudservicev1.DeleteApiKeyRequest) (*cloudservicev1.DeleteApiKeyResponse, error) {
			deleted = in
			return &cloudservicev1.DeleteApiKeyResponse{}, nil
		},
	}

	_, err := newServiceAccountAPIKeyBuilder(fake).Delete(context.Background(), &v2.ResourceId{ResourceType: serviceAccountAPIKeyResourceType.Id, Resource: "key-1"}, nil)
	require.NoError(t, err)
	require.NotNil(t, deleted, "a DELETE_FAILED key must be deleted again")
	require.Equal(t, "v3", deleted.GetResourceVersion())
}

// TestDeleteAPIKeyFailsFastWhenADeletingKeyFailsToDelete: a key observed as
// DELETING that moves to DELETE_FAILED fails the revoke at once instead of
// polling for the full deletion window.
func TestDeleteAPIKeyFailsFastWhenADeletingKeyFailsToDelete(t *testing.T) {
	t.Parallel()

	reads := 0
	fake := &fakeCloudService{
		getApiKey: func(context.Context, *cloudservicev1.GetApiKeyRequest) (*cloudservicev1.GetApiKeyResponse, error) {
			reads++
			state := resourcev1.ResourceState_RESOURCE_STATE_DELETING
			if reads > 1 {
				state = resourcev1.ResourceState_RESOURCE_STATE_DELETE_FAILED
			}
			return &cloudservicev1.GetApiKeyResponse{ApiKey: &identityv1.ApiKey{Id: "key-1", State: state, ResourceVersion: "v1"}}, nil
		},
		deleteApiKey: func(context.Context, *cloudservicev1.DeleteApiKeyRequest) (*cloudservicev1.DeleteApiKeyResponse, error) {
			t.Fatal("the wait must not issue a second delete")
			return nil, nil
		},
	}

	start := time.Now()
	_, err := newServiceAccountAPIKeyBuilder(fake).Delete(context.Background(), &v2.ResourceId{ResourceType: serviceAccountAPIKeyResourceType.Id, Resource: "key-1"}, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "DELETE_FAILED")
	require.Less(t, time.Since(start), time.Minute)
	require.Equal(t, 2, reads)
}
