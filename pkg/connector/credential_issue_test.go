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
	"github.com/conductorone/baton-sdk/pkg/connectorbuilder"
	rs "github.com/conductorone/baton-sdk/pkg/types/resource"

	"github.com/conductorone/baton-temporalcloud/pkg/client"

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
// rather than on the ConnectorServer interface, so the test asserts for it.
type capabilityProvider interface {
	GetCapabilities(context.Context) (*v2.ConnectorCapabilities, error)
}

// TestCapabilitiesAdvertiseRevocableAPIKey proves the connector's declaration
// survives the SDK's own validation. That check is the one that rejects an
// issuance descriptor whose secret resource type has no ResourceDeleterV2, so
// a passing capability read is evidence the api-key type really is revocable
// and not just advertised.
func TestCapabilitiesAdvertiseRevocableAPIKey(t *testing.T) {
	t.Parallel()

	// client.New is lazy: it builds a gRPC client without dialing, and
	// capability discovery makes no RPC, so no provider connection is needed.
	cloudClient, err := client.New("test-api-key")
	require.NoError(t, err)
	server, err := connectorbuilder.NewConnector(context.Background(), &Connector{cloudServiceClient: cloudClient})
	require.NoError(t, err)
	provider, ok := server.(capabilityProvider)
	require.True(t, ok, "connector must expose GetCapabilities")

	caps, err := provider.GetCapabilities(context.Background())
	require.NoError(t, err)

	var issue *v2.CredentialDetailsCredentialIssue
	declaredSecretType := false
	for _, rtc := range caps.GetResourceTypeCapabilities() {
		switch rtc.GetResourceType().GetId() {
		case serviceAccountResourceType.Id:
			issue = rtc.GetCredentialIssue()
		case apiKeyResourceType.Id:
			declaredSecretType = true
			require.Contains(t, rtc.GetResourceType().GetTraits(), v2.ResourceType_TRAIT_SECRET,
				"api-key must be declared as a secret resource type")
			require.Contains(t, rtc.GetCapabilities(), v2.Capability_CAPABILITY_RESOURCE_DELETE,
				"api-key must advertise a delete capability")
		}
	}

	require.True(t, declaredSecretType, "the api-key resource type must be declared")
	require.NotNil(t, issue, "the service-account resource type must advertise credential issuance")
	require.Equal(t, v2.CapabilityDetailCredentialOption_CAPABILITY_DETAIL_CREDENTIAL_OPTION_API_KEY, issue.GetPreferredOption())
	require.Len(t, issue.GetOptions(), 1)

	descriptor := issue.GetOptions()[0]
	require.Equal(t, apiKeyResourceType.Id, descriptor.GetSecretResourceTypeId())
	require.Equal(t, v2.CredentialResourceMode_CREDENTIAL_RESOURCE_MODE_DISCOVERABLE, descriptor.GetResourceMode())
	require.Equal(t, apiKeyMinTTL, descriptor.GetExpiry().GetMin().AsDuration())
	require.Equal(t, apiKeyMaxTTL, descriptor.GetExpiry().GetMax().AsDuration())
	require.True(t, capsHasCapability(caps, v2.Capability_CAPABILITY_CREDENTIAL_ISSUE))
}

func capsHasCapability(caps *v2.ConnectorCapabilities, want v2.Capability) bool {
	for _, got := range caps.GetConnectorCapabilities() {
		if got == want {
			return true
		}
	}
	return false
}

// TestIssueAPIKeyForServiceAccount covers the happy path: the key is created
// for the requested service account, owned by that account, bounded by the
// connector's default TTL, and returned as an api-key secret whose trait names
// the authenticating identity.
func TestIssueAPIKeyForServiceAccount(t *testing.T) {
	t.Parallel()

	var created *cloudservicev1.CreateApiKeyRequest
	fake := &fakeCloudService{
		getApiKeys: func(_ context.Context, in *cloudservicev1.GetApiKeysRequest) (*cloudservicev1.GetApiKeysResponse, error) {
			require.Equal(t, "sa-1", in.GetOwnerId())
			require.Equal(t, identityv1.OwnerType_OWNER_TYPE_SERVICE_ACCOUNT, in.GetOwnerType())
			return &cloudservicev1.GetApiKeysResponse{}, nil
		},
		createApiKey: func(_ context.Context, in *cloudservicev1.CreateApiKeyRequest) (*cloudservicev1.CreateApiKeyResponse, error) {
			created = in
			return &cloudservicev1.CreateApiKeyResponse{KeyId: "key-1", Token: "vended-secret"}, nil
		},
	}

	identity := &v2.ResourceId{ResourceType: serviceAccountResourceType.Id, Resource: "sa-1"}
	before := time.Now()
	out, err := newServiceAccountBuilder(fake).Issue(context.Background(), &connectorbuilder.CredentialIssueInput{
		IdentityID:        identity,
		CredentialOptions: apiKeyIssueOptions(),
		RequestID:         "req-1",
	})
	require.NoError(t, err)

	require.NotNil(t, created)
	spec := created.GetSpec()
	require.Equal(t, "sa-1", spec.GetOwnerId())
	require.Equal(t, identityv1.OwnerType_OWNER_TYPE_SERVICE_ACCOUNT, spec.GetOwnerType())
	require.Equal(t, "c1-req-1", spec.GetDisplayName())
	require.NotEmpty(t, spec.GetDescription())
	require.WithinDuration(t, before.Add(apiKeyDefaultTTL), spec.GetExpiryTime().AsTime(), time.Minute)

	require.NotNil(t, out)
	require.Equal(t, apiKeyResourceType.Id, out.Secret.GetId().GetResourceType())
	require.Equal(t, "key-1", out.Secret.GetId().GetResource())
	require.Equal(t, v2.CredentialResourceMode_CREDENTIAL_RESOURCE_MODE_DISCOVERABLE, out.ResourceMode)
	require.Len(t, out.PlaintextData, 1)
	require.Equal(t, "api_key", out.PlaintextData[0].GetName())
	require.Equal(t, "vended-secret", string(out.PlaintextData[0].GetBytes()))

	trait := secretTrait(t, out.Secret)
	require.True(t, proto.Equal(identity, trait.GetIdentityId()), "secret trait identity must be the requested service account")
	require.Equal(t, v2.SecretTrait_CREDENTIAL_TYPE_STATIC_SECRET, trait.GetCredentialType())
	require.Equal(t, apiKeyCredentialDetail, trait.GetCredentialDetail())
	require.NotNil(t, trait.GetExpiresAt())
	require.WithinDuration(t, spec.GetExpiryTime().AsTime(), trait.GetExpiresAt().AsTime(), time.Second)
	require.True(t, proto.Equal(identity, out.Secret.GetParentResourceId()))
}

// TestIssueAPIKeyHonoursRequestedExpiry proves a caller-selected expiry is
// passed through to the provider unchanged, and that the returned secret
// reports the same instant rather than a locally estimated one.
func TestIssueAPIKeyHonoursRequestedExpiry(t *testing.T) {
	t.Parallel()

	var created *cloudservicev1.CreateApiKeyRequest
	fake := &fakeCloudService{
		getApiKeys: func(context.Context, *cloudservicev1.GetApiKeysRequest) (*cloudservicev1.GetApiKeysResponse, error) {
			return &cloudservicev1.GetApiKeysResponse{}, nil
		},
		createApiKey: func(_ context.Context, in *cloudservicev1.CreateApiKeyRequest) (*cloudservicev1.CreateApiKeyResponse, error) {
			created = in
			return &cloudservicev1.CreateApiKeyResponse{KeyId: "key-2", Token: "tok"}, nil
		},
	}

	requested := time.Now().Add(6 * time.Hour).Truncate(time.Second)
	out, err := newServiceAccountBuilder(fake).Issue(context.Background(), &connectorbuilder.CredentialIssueInput{
		IdentityID:        &v2.ResourceId{ResourceType: serviceAccountResourceType.Id, Resource: "sa-1"},
		CredentialOptions: apiKeyIssueOptions(),
		ExpiresAt:         timestamppb.New(requested),
		RequestID:         "req-2",
	})
	require.NoError(t, err)

	require.True(t, requested.Equal(created.GetSpec().GetExpiryTime().AsTime()), "requested expiry must reach the provider unchanged")
	require.True(t, requested.Equal(secretTrait(t, out.Secret).GetExpiresAt().AsTime()))
}

// TestIssueAPIKeyRejectsRequestedExpiryOutsideProviderLimits pins the bound the
// connector enforces itself, independently of the SDK's request-time check.
func TestIssueAPIKeyRejectsRequestedExpiryOutsideProviderLimits(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name      string
		expiresAt time.Time
	}{
		{name: "beyond provider maximum", expiresAt: time.Now().Add(apiKeyMaxTTL + 24*time.Hour)},
		{name: "below connector minimum", expiresAt: time.Now().Add(30 * time.Second)},
		{name: "in the past", expiresAt: time.Now().Add(-time.Hour)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			fake := &fakeCloudService{
				getApiKeys: func(context.Context, *cloudservicev1.GetApiKeysRequest) (*cloudservicev1.GetApiKeysResponse, error) {
					return &cloudservicev1.GetApiKeysResponse{}, nil
				},
				createApiKey: func(context.Context, *cloudservicev1.CreateApiKeyRequest) (*cloudservicev1.CreateApiKeyResponse, error) {
					t.Fatal("no key may be created for an out-of-range expiry")
					return nil, nil
				},
			}
			_, err := newServiceAccountBuilder(fake).Issue(context.Background(), &connectorbuilder.CredentialIssueInput{
				IdentityID:        &v2.ResourceId{ResourceType: serviceAccountResourceType.Id, Resource: "sa-1"},
				CredentialOptions: apiKeyIssueOptions(),
				ExpiresAt:         timestamppb.New(tc.expiresAt),
				RequestID:         "req-bounds",
			})
			require.Equal(t, codes.InvalidArgument, status.Code(err))
		})
	}
}

// TestIssueAPIKeyRefusesDuplicateRequest covers retry safety: a request whose
// provider-side key already exists must not mint a second one.
func TestIssueAPIKeyRefusesDuplicateRequest(t *testing.T) {
	t.Parallel()

	fake := &fakeCloudService{
		getApiKeys: func(context.Context, *cloudservicev1.GetApiKeysRequest) (*cloudservicev1.GetApiKeysResponse, error) {
			return &cloudservicev1.GetApiKeysResponse{ApiKeys: []*identityv1.ApiKey{
				{Id: "key-existing", Spec: &identityv1.ApiKeySpec{OwnerId: "sa-1", DisplayName: "c1-req-dup"}},
			}}, nil
		},
		createApiKey: func(context.Context, *cloudservicev1.CreateApiKeyRequest) (*cloudservicev1.CreateApiKeyResponse, error) {
			t.Fatal("a duplicate request must not create a second key")
			return nil, nil
		},
	}

	_, err := newServiceAccountBuilder(fake).Issue(context.Background(), &connectorbuilder.CredentialIssueInput{
		IdentityID:        &v2.ResourceId{ResourceType: serviceAccountResourceType.Id, Resource: "sa-1"},
		CredentialOptions: apiKeyIssueOptions(),
		RequestID:         "req-dup",
	})
	require.Equal(t, codes.AlreadyExists, status.Code(err))
	require.ErrorContains(t, err, "key-existing")
}

// TestIssueAPIKeyRejectsUnexpectedRequests keeps the arm honest for a caller
// that reaches Issue directly rather than through the SDK's validation.
func TestIssueAPIKeyRejectsUnexpectedRequests(t *testing.T) {
	t.Parallel()

	builder := newServiceAccountBuilder(&fakeCloudService{})

	t.Run("non-service-account identity", func(t *testing.T) {
		t.Parallel()
		_, err := builder.Issue(context.Background(), &connectorbuilder.CredentialIssueInput{
			IdentityID:        &v2.ResourceId{ResourceType: userResourceType.Id, Resource: "u-1"},
			CredentialOptions: apiKeyIssueOptions(),
			RequestID:         "req-3",
		})
		require.Equal(t, codes.InvalidArgument, status.Code(err))
	})

	t.Run("unknown secret resource type", func(t *testing.T) {
		t.Parallel()
		_, err := builder.Issue(context.Background(), &connectorbuilder.CredentialIssueInput{
			IdentityID: &v2.ResourceId{ResourceType: serviceAccountResourceType.Id, Resource: "sa-1"},
			CredentialOptions: v2.CredentialIssueOptions_builder{
				SecretResourceTypeId: "something-else",
				ApiKey:               v2.CredentialIssueOptions_ApiKey_builder{}.Build(),
			}.Build(),
			RequestID: "req-4",
		})
		require.Equal(t, codes.InvalidArgument, status.Code(err))
	})
}

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

	_, err := newAPIKeyBuilder(fake).Delete(context.Background(), &v2.ResourceId{ResourceType: apiKeyResourceType.Id, Resource: "key-1"}, nil)
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

	_, err := newAPIKeyBuilder(fake).Delete(context.Background(), &v2.ResourceId{ResourceType: apiKeyResourceType.Id, Resource: "key-gone"}, nil)
	require.NoError(t, err)
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

	_, err := newAPIKeyBuilder(fake).Delete(context.Background(), &v2.ResourceId{ResourceType: apiKeyResourceType.Id, Resource: "key-1"}, nil)
	require.Equal(t, codes.PermissionDenied, status.Code(err))
}

// TestListAPIKeysSkipsTerminalStates proves the synced inventory carries the
// handle, owner and expiry the delete and audit paths need, and that keys the
// provider can no longer authenticate are not resurrected as live resources.
func TestListAPIKeysSkipsTerminalStates(t *testing.T) {
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
				{Id: "key-deleted", State: resourcev1.ResourceState_RESOURCE_STATE_DELETED, Spec: &identityv1.ApiKeySpec{OwnerId: "sa-1"}},
				{Id: "key-expired", State: resourcev1.ResourceState_RESOURCE_STATE_EXPIRED, Spec: &identityv1.ApiKeySpec{OwnerId: "sa-1"}},
			}}, nil
		},
	}

	resources, _, err := newAPIKeyBuilder(fake).List(context.Background(), nil, rs.SyncOpAttrs{})
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

func apiKeyIssueOptions() *v2.CredentialIssueOptions {
	return v2.CredentialIssueOptions_builder{
		SecretResourceTypeId: apiKeyResourceType.Id,
		ApiKey:               v2.CredentialIssueOptions_ApiKey_builder{}.Build(),
	}.Build()
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
