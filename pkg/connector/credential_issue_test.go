package connector

import (
	"context"
	"sync"
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

// issuanceFake is a provider stub for the issuance path with every answer
// defaulted to a successful, verified mint. A test overrides only the answer it
// is about.
type issuanceFake struct {
	*fakeCloudService

	ownerID   string
	expiresAt time.Time
	token     string
	keyID     string

	// Provider answers a test may override.
	opState    operationv1.AsyncOperation_State
	opFound    bool
	existing   []*identityv1.ApiKey
	record     *identityv1.ApiKey
	createErr  error
	noSecret   bool
	createGate func()
	created    []*cloudservicev1.CreateApiKeyRequest
	deleted    []*cloudservicev1.DeleteApiKeyRequest
}

func newIssuanceFake(t *testing.T, ownerID string, expiresAt time.Time) *issuanceFake {
	t.Helper()
	f := &issuanceFake{
		ownerID:   ownerID,
		expiresAt: expiresAt,
		token:     "vended-secret",
		keyID:     "key-1",
	}
	f.fakeCloudService = &fakeCloudService{
		getAsyncOp: func(_ context.Context, in *cloudservicev1.GetAsyncOperationRequest) (*cloudservicev1.GetAsyncOperationResponse, error) {
			require.NotEmpty(t, in.GetAsyncOperationId(), "every issuance must carry a request identity")
			if !f.opFound {
				return nil, status.Error(codes.NotFound, "operation not found")
			}
			return &cloudservicev1.GetAsyncOperationResponse{AsyncOperation: &operationv1.AsyncOperation{
				Id:    in.GetAsyncOperationId(),
				State: f.opState,
			}}, nil
		},
		getApiKeys: func(context.Context, *cloudservicev1.GetApiKeysRequest) (*cloudservicev1.GetApiKeysResponse, error) {
			return &cloudservicev1.GetApiKeysResponse{ApiKeys: f.existing}, nil
		},
		createApiKey: func(_ context.Context, in *cloudservicev1.CreateApiKeyRequest) (*cloudservicev1.CreateApiKeyResponse, error) {
			f.created = append(f.created, in)
			if f.createGate != nil {
				f.createGate()
			}
			if f.createErr != nil {
				return nil, f.createErr
			}
			token := f.token
			if f.noSecret {
				token = ""
			}
			return &cloudservicev1.CreateApiKeyResponse{KeyId: f.keyID, Token: token}, nil
		},
		getApiKey: func(_ context.Context, in *cloudservicev1.GetApiKeyRequest) (*cloudservicev1.GetApiKeyResponse, error) {
			if f.record != nil {
				return &cloudservicev1.GetApiKeyResponse{ApiKey: f.record}, nil
			}
			// A provider that accepted the create is the case these tests are
			// not about, so the readback echoes what was asked for. A test that
			// wants a disagreement sets f.record.
			spec := &identityv1.ApiKeySpec{
				OwnerId:     f.ownerID,
				OwnerType:   identityv1.OwnerType_OWNER_TYPE_SERVICE_ACCOUNT,
				DisplayName: issuedCredentialName("req-1"),
			}
			if n := len(f.created); n > 0 {
				spec = proto.Clone(f.created[n-1].GetSpec()).(*identityv1.ApiKeySpec)
			} else if !f.expiresAt.IsZero() {
				spec.ExpiryTime = timestamppb.New(f.expiresAt)
			}
			return &cloudservicev1.GetApiKeyResponse{ApiKey: &identityv1.ApiKey{
				Id:              in.GetKeyId(),
				ResourceVersion: "v1",
				Spec:            spec,
			}}, nil
		},
		deleteApiKey: func(ctx context.Context, in *cloudservicev1.DeleteApiKeyRequest) (*cloudservicev1.DeleteApiKeyResponse, error) {
			// The whole point of the rollback context: a delete that ran on the
			// caller's context would never leave the process when the caller is
			// already cancelled or past its deadline, silently leaving the key
			// behind. Asserting here makes that regression fail loudly.
			require.NoError(t, ctx.Err(), "cleanup must not run on an already-cancelled caller context")
			f.deleted = append(f.deleted, in)
			return &cloudservicev1.DeleteApiKeyResponse{}, nil
		},
	}
	return f
}

func (f *issuanceFake) issue(ctx context.Context, requestID string) (*connectorbuilder.CredentialIssueOutput, error) {
	return newServiceAccountBuilder(f.fakeCloudService).Issue(ctx, &connectorbuilder.CredentialIssueInput{
		IdentityID:        &v2.ResourceId{ResourceType: serviceAccountResourceType.Id, Resource: f.ownerID},
		CredentialOptions: apiKeyIssueOptions(),
		RequestID:         requestID,
	})
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
// connector's default TTL, verified against the provider's own record, and
// returned as an api-key secret whose trait names the authenticating identity.
func TestIssueAPIKeyForServiceAccount(t *testing.T) {
	t.Parallel()

	before := time.Now()
	fake := newIssuanceFake(t, "sa-1", before.Add(apiKeyDefaultTTL).Truncate(time.Second))
	out, err := fake.issue(context.Background(), "req-1")
	require.NoError(t, err)

	require.Len(t, fake.created, 1)
	spec := fake.created[0].GetSpec()
	require.Equal(t, "sa-1", spec.GetOwnerId())
	require.Equal(t, identityv1.OwnerType_OWNER_TYPE_SERVICE_ACCOUNT, spec.GetOwnerType())
	require.Equal(t, "c1-req-1", spec.GetDisplayName())
	require.NotEmpty(t, spec.GetDescription())
	require.WithinDuration(t, before.Add(apiKeyDefaultTTL), spec.GetExpiryTime().AsTime(), time.Minute)
	require.Equal(t, apiKeyOperationID("req-1"), fake.created[0].GetAsyncOperationId(),
		"the create must carry the request identity a retry can observe")

	require.NotNil(t, out)
	require.Equal(t, apiKeyResourceType.Id, out.Secret.GetId().GetResourceType())
	require.Equal(t, "key-1", out.Secret.GetId().GetResource())
	require.Equal(t, v2.CredentialResourceMode_CREDENTIAL_RESOURCE_MODE_DISCOVERABLE, out.ResourceMode)
	require.Len(t, out.PlaintextData, 1)
	require.Equal(t, apiKeyV2ContentType, out.PlaintextData[0].GetName())
	// The literal is the assertion: the document is the pinned schema's flat
	// JSON object, in declaration order, with the empty optionals omitted. A
	// change to the encoder must fail here rather than quietly re-shape what a
	// client decodes.
	require.Equal(t, `{"key_value":"vended-secret","key_id":"key-1"}`, string(out.PlaintextData[0].GetBytes()))

	identity := &v2.ResourceId{ResourceType: serviceAccountResourceType.Id, Resource: "sa-1"}
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

	requested := time.Now().Add(6 * time.Hour).Truncate(time.Second)
	fake := newIssuanceFake(t, "sa-1", requested)
	out, err := newServiceAccountBuilder(fake.fakeCloudService).Issue(context.Background(), &connectorbuilder.CredentialIssueInput{
		IdentityID:        &v2.ResourceId{ResourceType: serviceAccountResourceType.Id, Resource: "sa-1"},
		CredentialOptions: apiKeyIssueOptions(),
		ExpiresAt:         timestamppb.New(requested),
		RequestID:         "req-2",
	})
	require.NoError(t, err)

	require.True(t, requested.Equal(fake.created[0].GetSpec().GetExpiryTime().AsTime()),
		"requested expiry must reach the provider unchanged")
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

			fake := newIssuanceFake(t, "sa-1", tc.expiresAt)
			_, err := newServiceAccountBuilder(fake.fakeCloudService).Issue(context.Background(), &connectorbuilder.CredentialIssueInput{
				IdentityID:        &v2.ResourceId{ResourceType: serviceAccountResourceType.Id, Resource: "sa-1"},
				CredentialOptions: apiKeyIssueOptions(),
				ExpiresAt:         timestamppb.New(tc.expiresAt),
				RequestID:         "req-bounds",
			})
			require.Equal(t, codes.InvalidArgument, status.Code(err))
			require.Empty(t, fake.created, "no key may be created for an out-of-range expiry")
		})
	}
}

// TestIssueAPIKeyRefusesDuplicateRequest covers the retry case where the
// provider's key listing already shows the predecessor's key.
func TestIssueAPIKeyRefusesDuplicateRequest(t *testing.T) {
	t.Parallel()

	fake := newIssuanceFake(t, "sa-1", time.Now().Add(time.Hour))
	fake.existing = []*identityv1.ApiKey{
		{Id: "key-existing", Spec: &identityv1.ApiKeySpec{OwnerId: "sa-1", DisplayName: "c1-req-dup"}},
	}
	_, err := fake.issue(context.Background(), "req-dup")
	require.Equal(t, codes.AlreadyExists, status.Code(err))
	require.ErrorContains(t, err, "key-existing")
	require.Empty(t, fake.created, "a duplicate request must not create a second key")
	require.Empty(t, fake.deleted,
		"the existing key must not be deleted: the connector cannot prove its secret was never delivered")
}

// TestIssueAPIKeyRefusesWhenOperationAlreadyFulfilled covers the case the key
// listing cannot see: a previous attempt created the key, its response was
// lost, and the provider's listing has not caught up. The provider's own
// operation record is the signal that survives that.
func TestIssueAPIKeyRefusesWhenOperationAlreadyFulfilled(t *testing.T) {
	t.Parallel()

	fake := newIssuanceFake(t, "sa-1", time.Now().Add(time.Hour))
	fake.opFound = true
	fake.opState = operationv1.AsyncOperation_STATE_FULFILLED
	// The listing lags: it does not show the key the operation created.
	fake.existing = nil

	_, err := fake.issue(context.Background(), "req-lost")
	require.Equal(t, codes.AlreadyExists, status.Code(err))
	require.ErrorContains(t, err, "no key for it was found",
		"the error must say the handle was unavailable rather than invent one")
	require.ErrorContains(t, err, "raise a new request",
		"the request id is spent, so the recovery is a new request")
	require.NotContains(t, err.Error(), "revoke",
		"with no key located there is nothing to revoke, and saying otherwise sends an operator hunting")
	require.Empty(t, fake.created, "a fulfilled operation must not be followed by a second create")
	require.Empty(t, fake.deleted,
		"the existing key must not be deleted: the connector cannot prove its secret was never delivered")
}

// TestIssueAPIKeyDuplicateErrorNamesTheKeyAndTheRecovery is the other half of
// the guidance: when a key IS located, the operator is told to deal with that
// key first, and that the request id is spent either way.
func TestIssueAPIKeyDuplicateErrorNamesTheKeyAndTheRecovery(t *testing.T) {
	t.Parallel()

	fake := newIssuanceFake(t, "sa-1", time.Now().Add(time.Hour))
	fake.existing = []*identityv1.ApiKey{
		{Id: "key-existing", Spec: &identityv1.ApiKeySpec{OwnerId: "sa-1", DisplayName: "c1-req-named"}},
	}

	_, err := fake.issue(context.Background(), "req-named")
	require.Equal(t, codes.AlreadyExists, status.Code(err))
	require.ErrorContains(t, err, "key-existing", "the operator needs the key's id to act on it")
	require.ErrorContains(t, err, "request id is spent")
	require.ErrorContains(t, err, "raise a new request")
	require.Empty(t, fake.created)
	require.Empty(t, fake.deleted)
}

// TestIssueAPIKeyRefusesWhenOperationInFlight covers the concurrent case from
// the connector's side: another attempt for the same request is mid-flight, so
// this one must not mint a second key.
func TestIssueAPIKeyRefusesWhenOperationInFlight(t *testing.T) {
	t.Parallel()

	fake := newIssuanceFake(t, "sa-1", time.Now().Add(time.Hour))
	fake.opFound = true
	fake.opState = operationv1.AsyncOperation_STATE_IN_PROGRESS

	_, err := fake.issue(context.Background(), "req-inflight")
	require.Equal(t, codes.AlreadyExists, status.Code(err))
	require.Empty(t, fake.created)
}

// TestIssueAPIKeyRefusesAfterAFailedOrCancelledOperation proves a failed or
// cancelled attempt does not license a re-mint.
//
// The operation record proves an earlier attempt existed; its state does not
// prove the provider created no key, because a create can commit and then
// report a failure. The key listing is empty here, and deliberately not
// consulted for permission: a listing that can lag supplies no proof either. So
// the request is refused with neither a create nor a delete -- nothing is
// minted, and nothing is revoked.
func TestIssueAPIKeyRefusesAfterAFailedOrCancelledOperation(t *testing.T) {
	t.Parallel()

	for _, state := range []struct {
		name  string
		state operationv1.AsyncOperation_State
	}{
		{name: "failed", state: operationv1.AsyncOperation_STATE_FAILED},
		{name: "cancelled", state: operationv1.AsyncOperation_STATE_CANCELLED},
		{name: "unspecified", state: operationv1.AsyncOperation_STATE_UNSPECIFIED},
	} {
		t.Run(state.name, func(t *testing.T) {
			t.Parallel()

			fake := newIssuanceFake(t, "sa-1", time.Now().Add(time.Hour))
			fake.opFound = true
			fake.opState = state.state
			fake.existing = nil // the listing is empty

			_, err := fake.issue(context.Background(), "req-"+state.name)
			require.Equal(t, codes.AlreadyExists, status.Code(err))
			require.Empty(t, fake.created, "a recorded operation must not be followed by a create")
			require.Empty(t, fake.deleted, "refusing must not revoke anything")
		})
	}
}

// TestIssueAPIKeyFailsClosedOnAmbiguousProviderAnswers characterizes the
// conservative outcome for every interleaving where the provider gives no
// verdict. In each case nothing is minted, and the caller is told why: an
// unreadable record is never treated as an absent one, and a create whose
// outcome is unknown never triggers a blind delete.
func TestIssueAPIKeyFailsClosedOnAmbiguousProviderAnswers(t *testing.T) {
	t.Parallel()

	t.Run("operation record unreadable", func(t *testing.T) {
		t.Parallel()

		fake := newIssuanceFake(t, "sa-1", time.Now().Add(time.Hour))
		fake.getAsyncOp = func(context.Context, *cloudservicev1.GetAsyncOperationRequest) (*cloudservicev1.GetAsyncOperationResponse, error) {
			return nil, status.Error(codes.Unavailable, "provider unreachable")
		}
		_, err := fake.issue(context.Background(), "req-op-unknown")
		require.Equal(t, codes.Unavailable, status.Code(err))
		require.Empty(t, fake.created, "an unreadable operation record must not be read as absent")
	})

	t.Run("key listing unreadable", func(t *testing.T) {
		t.Parallel()

		fake := newIssuanceFake(t, "sa-1", time.Now().Add(time.Hour))
		fake.getApiKeys = func(context.Context, *cloudservicev1.GetApiKeysRequest) (*cloudservicev1.GetApiKeysResponse, error) {
			return nil, status.Error(codes.Unavailable, "provider unreachable")
		}
		_, err := fake.issue(context.Background(), "req-list-unknown")
		require.Equal(t, codes.Unavailable, status.Code(err))
		require.Empty(t, fake.created, "an unreadable listing must not be read as empty")
	})

	t.Run("create outcome unknown", func(t *testing.T) {
		t.Parallel()

		fake := newIssuanceFake(t, "sa-1", time.Now().Add(time.Hour))
		fake.createErr = status.Error(codes.DeadlineExceeded, "create timed out")
		_, err := fake.issue(context.Background(), "req-ambiguous-create")
		require.Equal(t, codes.DeadlineExceeded, status.Code(err))
		require.ErrorContains(t, err, "failed to create service account API key",
			"the failure must name the operation that is now uncertain")
		require.Empty(t, fake.deleted,
			"a create of unknown outcome must not trigger a blind delete: no key id is known, and the key may not exist")
	})
}

// TestIssueAPIKeyRetriesTheReadbackOfAnUnawaitedCreate covers the race between
// the create and the read-back that follows it.
//
// The create is deliberately not awaited, so the read can see NotFound for a key
// the provider has already returned an id for. Treating that as a failure would
// roll the key back -- destroying a valid credential, spending the request id,
// and, if the rollback's delete also raced the write, leaving the key to appear
// later as an untracked credential.
func TestIssueAPIKeyRetriesTheReadbackOfAnUnawaitedCreate(t *testing.T) {
	t.Parallel()

	expiresAt := time.Now().Add(4 * time.Hour).Truncate(time.Second)
	fake := newIssuanceFake(t, "sa-1", expiresAt)
	inner := fake.getApiKey
	notFounds := 0
	fake.getApiKey = func(c context.Context, in *cloudservicev1.GetApiKeyRequest) (*cloudservicev1.GetApiKeyResponse, error) {
		if notFounds < 2 {
			notFounds++
			return nil, status.Error(codes.NotFound, "key not visible yet")
		}
		return inner(c, in)
	}

	out, err := newServiceAccountBuilder(fake.fakeCloudService).Issue(context.Background(), &connectorbuilder.CredentialIssueInput{
		IdentityID:        &v2.ResourceId{ResourceType: serviceAccountResourceType.Id, Resource: "sa-1"},
		CredentialOptions: apiKeyIssueOptions(),
		ExpiresAt:         timestamppb.New(expiresAt),
		RequestID:         "req-readback-race",
	})
	require.NoError(t, err, "a read-back briefly behind the write must not fail the issuance")
	require.NotNil(t, out)
	require.Equal(t, 2, notFounds, "the retry must actually have been exercised")
	require.Empty(t, fake.deleted, "no key may be rolled back for a read that merely raced the write")
}

// TestIssueAPIKeyDoesNotRetryOtherReadbackErrors keeps the retry narrow: only
// NotFound is a race. A permission or transport failure is the provider's
// answer and must fail immediately rather than be retried into a slow timeout.
func TestIssueAPIKeyDoesNotRetryOtherReadbackErrors(t *testing.T) {
	t.Parallel()

	expiresAt := time.Now().Add(4 * time.Hour).Truncate(time.Second)
	fake := newIssuanceFake(t, "sa-1", expiresAt)
	calls := 0
	fake.getApiKey = func(context.Context, *cloudservicev1.GetApiKeyRequest) (*cloudservicev1.GetApiKeyResponse, error) {
		calls++
		return nil, status.Error(codes.PermissionDenied, "not authorized to read the key")
	}

	_, err := newServiceAccountBuilder(fake.fakeCloudService).Issue(context.Background(), &connectorbuilder.CredentialIssueInput{
		IdentityID:        &v2.ResourceId{ResourceType: serviceAccountResourceType.Id, Resource: "sa-1"},
		CredentialOptions: apiKeyIssueOptions(),
		ExpiresAt:         timestamppb.New(expiresAt),
		RequestID:         "req-readback-denied",
	})
	require.Error(t, err)
	require.Equal(t, 1, calls, "a non-NotFound read error must not be retried")
	require.Len(t, fake.deleted, 1, "an unverifiable key is still rolled back")
}

// TestIssueAPIKeyConcurrentAttemptsShareOneRequestIdentity pins the one thing
// the connector controls about concurrency.
//
// Both attempts are gated so both pass the pre-checks before either creates --
// the worst case for a list-then-create guard. The connector cannot make the
// two creates atomic; what it can do is make them indistinguishable to the
// provider by sending the same request identity. Whether the provider collapses
// them is the provider's contract, and Temporal Cloud documents no such
// guarantee: this test asserts the invariant the connector owns, not a
// deduplication the connector cannot enforce.
func TestIssueAPIKeyConcurrentAttemptsShareOneRequestIdentity(t *testing.T) {
	t.Parallel()

	var barrier sync.WaitGroup
	barrier.Add(2)
	fake := newIssuanceFake(t, "sa-1", time.Now().Add(time.Hour))
	fake.createGate = func() { barrier.Done(); barrier.Wait() }

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = fake.issue(context.Background(), "req-concurrent")
		}()
	}
	wg.Wait()

	require.Len(t, fake.created, 2, "both gated attempts reach the provider")
	require.Equal(t, fake.created[0].GetAsyncOperationId(), fake.created[1].GetAsyncOperationId(),
		"concurrent attempts must present the same request identity to the provider")
	require.Equal(t, fake.created[0].GetSpec().GetDisplayName(), fake.created[1].GetSpec().GetDisplayName())
	require.Equal(t, apiKeyOperationID("req-concurrent"), fake.created[0].GetAsyncOperationId())
	for _, err := range errs {
		require.NoError(t, err)
	}
}

// TestIssueAPIKeyRejectsUnverifiedProviderRecord proves the connector does not
// trust what it asked for: the provider's own readback must agree on owner and
// expiry before a credential is returned, and a disagreement rolls the key back.
func TestIssueAPIKeyRejectsUnverifiedProviderRecord(t *testing.T) {
	t.Parallel()

	expiresAt := time.Now().Add(4 * time.Hour).Truncate(time.Second)

	for _, tc := range []struct {
		name   string
		record *identityv1.ApiKey
	}{
		{
			name: "owner mismatch",
			record: &identityv1.ApiKey{Id: "key-1", Spec: &identityv1.ApiKeySpec{
				OwnerId: "someone-else", OwnerType: identityv1.OwnerType_OWNER_TYPE_SERVICE_ACCOUNT,
				ExpiryTime: timestamppb.New(expiresAt),
			}},
		},
		{
			name: "owner is not a service account",
			record: &identityv1.ApiKey{Id: "key-1", Spec: &identityv1.ApiKeySpec{
				OwnerId: "sa-1", OwnerType: identityv1.OwnerType_OWNER_TYPE_USER,
				ExpiryTime: timestamppb.New(expiresAt),
			}},
		},
		{
			name: "expiry ignored by provider",
			record: &identityv1.ApiKey{Id: "key-1", Spec: &identityv1.ApiKeySpec{
				OwnerId: "sa-1", OwnerType: identityv1.OwnerType_OWNER_TYPE_SERVICE_ACCOUNT,
				ExpiryTime: timestamppb.New(expiresAt.Add(30 * 24 * time.Hour)),
			}},
		},
		{
			// Thirty seconds past the approved deadline: inside the old
			// two-sided tolerance, and still a credential that outlives what
			// was approved. The comparison must be one-sided.
			name: "expiry marginally beyond the approved deadline",
			record: &identityv1.ApiKey{Id: "key-1", Spec: &identityv1.ApiKeySpec{
				OwnerId: "sa-1", OwnerType: identityv1.OwnerType_OWNER_TYPE_SERVICE_ACCOUNT,
				ExpiryTime: timestamppb.New(expiresAt.Add(30 * time.Second)),
			}},
		},
		{
			name: "expiry materially earlier than requested",
			record: &identityv1.ApiKey{Id: "key-1", Spec: &identityv1.ApiKeySpec{
				OwnerId: "sa-1", OwnerType: identityv1.OwnerType_OWNER_TYPE_SERVICE_ACCOUNT,
				ExpiryTime: timestamppb.New(expiresAt.Add(-5 * time.Minute)),
			}},
		},
		{
			name: "no expiry recorded",
			record: &identityv1.ApiKey{Id: "key-1", Spec: &identityv1.ApiKeySpec{
				OwnerId: "sa-1", OwnerType: identityv1.OwnerType_OWNER_TYPE_SERVICE_ACCOUNT,
			}},
		},
		{
			// AsTime normalizes malformed nanos rather than reporting them, so
			// an invalid timestamp must be rejected before it is treated as the
			// provider's authoritative instant.
			//
			// Nanos == 1e9 is invalid, and time.Unix normalizes it to exactly
			// the approved instant -- so this case is INSIDE the expiry
			// tolerance and only CheckValid can reject it. A timestamp far in
			// the future would be rejected by the comparison anyway and would
			// not cover the guard at all.
			name: "invalid expiry timestamp",
			record: &identityv1.ApiKey{Id: "key-1", Spec: &identityv1.ApiKeySpec{
				OwnerId: "sa-1", OwnerType: identityv1.OwnerType_OWNER_TYPE_SERVICE_ACCOUNT,
				ExpiryTime: &timestamppb.Timestamp{Seconds: expiresAt.Unix() - 1, Nanos: 1_000_000_000},
			}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			fake := newIssuanceFake(t, "sa-1", expiresAt)
			fake.record = tc.record
			out, err := newServiceAccountBuilder(fake.fakeCloudService).Issue(context.Background(), &connectorbuilder.CredentialIssueInput{
				IdentityID:        &v2.ResourceId{ResourceType: serviceAccountResourceType.Id, Resource: "sa-1"},
				CredentialOptions: apiKeyIssueOptions(),
				ExpiresAt:         timestamppb.New(expiresAt),
				RequestID:         "req-unverified",
			})
			require.Error(t, err)
			require.Nil(t, out, "an unverified provider record must not be returned as a credential")
			require.Len(t, fake.deleted, 1, "the unverifiable key must be rolled back")
			require.Equal(t, "key-1", fake.deleted[0].GetKeyId())
		})
	}
}

// TestIssueAPIKeyRollsBackWhenProviderReturnsNoSecret covers a response that
// names a key but carries no secret: the key is unusable and undeliverable, so
// it is removed rather than left holding one of the account's limited slots.
func TestIssueAPIKeyRollsBackWhenProviderReturnsNoSecret(t *testing.T) {
	t.Parallel()

	fake := newIssuanceFake(t, "sa-1", time.Now().Add(time.Hour))
	fake.noSecret = true

	out, err := fake.issue(context.Background(), "req-nosecret")
	require.Error(t, err)
	require.Nil(t, out)
	require.Len(t, fake.deleted, 1)
	require.Equal(t, "key-1", fake.deleted[0].GetKeyId())
	require.NotContains(t, err.Error(), "vended-secret", "an error must never carry credential material")
}

// TestIssueAPIKeyRollbackSurvivesCanceledContext proves cleanup is attempted
// even when the caller's context is already done, and that the original cause
// survives a cleanup failure instead of being replaced by it.
//
// The context is cancelled at the read-back, which is how this happens in
// production: the request expires, the read-back fails, and cleanup runs on a
// context that is already dead. The fake asserts the delete's context is live,
// so passing the caller's context through would fail the first subtest.
func TestIssueAPIKeyRollbackSurvivesCanceledContext(t *testing.T) {
	t.Parallel()

	t.Run("cleanup succeeds despite a canceled caller", func(t *testing.T) {
		t.Parallel()

		ctx, cancel := context.WithCancel(context.Background())
		fake := newIssuanceFake(t, "sa-1", time.Now().Add(time.Hour))
		// A provider record that disagrees with the request forces the
		// rollback path.
		fake.record = &identityv1.ApiKey{Id: "key-1", Spec: &identityv1.ApiKeySpec{
			OwnerId:   "someone-else",
			OwnerType: identityv1.OwnerType_OWNER_TYPE_SERVICE_ACCOUNT,
		}}
		inner := fake.getApiKey
		fake.getApiKey = func(c context.Context, in *cloudservicev1.GetApiKeyRequest) (*cloudservicev1.GetApiKeyResponse, error) {
			resp, err := inner(c, in)
			cancel() // the request expires here
			return resp, err
		}

		_, err := fake.issue(ctx, "req-canceled")
		require.Error(t, err)
		require.Len(t, fake.deleted, 1, "cleanup must not be abandoned because the caller gave up")
		require.Equal(t, "key-1", fake.deleted[0].GetKeyId())
	})

	t.Run("cleanup failure preserves the original cause", func(t *testing.T) {
		t.Parallel()

		fake := newIssuanceFake(t, "sa-1", time.Now().Add(time.Hour))
		fake.noSecret = true
		fake.deleteApiKey = func(context.Context, *cloudservicev1.DeleteApiKeyRequest) (*cloudservicev1.DeleteApiKeyResponse, error) {
			return nil, status.Error(codes.PermissionDenied, "not authorized to delete")
		}

		_, err := fake.issue(context.Background(), "req-cleanupfail")
		require.Error(t, err)
		require.ErrorContains(t, err, "provider returned an API key with no secret",
			"the original cause must survive a cleanup failure")
		require.ErrorContains(t, err, "may remain at the provider",
			"the caller must be told a key may have been left behind")
	})
}

// TestIssueAPIKeyReportsTheProviderExpiry proves the returned credential
// carries the expiry the provider actually recorded, not the one that was
// requested. Reporting the requested instant would assert an expiry the
// provider never confirmed.
func TestIssueAPIKeyReportsTheProviderExpiry(t *testing.T) {
	t.Parallel()

	requested := time.Now().Add(4 * time.Hour).Truncate(time.Second)
	// The provider rounds down by 30 seconds: accepted, and what must be
	// reported back.
	providerExpiry := requested.Add(-30 * time.Second)
	fake := newIssuanceFake(t, "sa-1", requested)
	fake.record = &identityv1.ApiKey{Id: "key-1", Spec: &identityv1.ApiKeySpec{
		OwnerId:     "sa-1",
		OwnerType:   identityv1.OwnerType_OWNER_TYPE_SERVICE_ACCOUNT,
		DisplayName: issuedCredentialName("req-actual"),
		ExpiryTime:  timestamppb.New(providerExpiry),
	}}

	out, err := newServiceAccountBuilder(fake.fakeCloudService).Issue(context.Background(), &connectorbuilder.CredentialIssueInput{
		IdentityID:        &v2.ResourceId{ResourceType: serviceAccountResourceType.Id, Resource: "sa-1"},
		CredentialOptions: apiKeyIssueOptions(),
		ExpiresAt:         timestamppb.New(requested),
		RequestID:         "req-actual",
	})
	require.NoError(t, err)
	require.Empty(t, fake.deleted, "an accepted rounding must not roll the key back")

	trait := secretTrait(t, out.Secret)
	require.True(t, providerExpiry.Equal(trait.GetExpiresAt().AsTime()),
		"the credential must report the provider's expiry, not the requested one")
	require.False(t, trait.GetExpiresAt().AsTime().After(requested),
		"the reported expiry must never exceed the approved deadline")
}

// TestIssueAPIKeyRejectsUnexpectedRequests keeps the arm honest for a caller
// that reaches Issue directly rather than through the SDK's validation.
func TestIssueAPIKeyRejectsUnexpectedRequests(t *testing.T) {
	t.Parallel()

	fake := newIssuanceFake(t, "sa-1", time.Now().Add(time.Hour))

	t.Run("non-service-account identity", func(t *testing.T) {
		t.Parallel()
		_, err := newServiceAccountBuilder(fake.fakeCloudService).Issue(context.Background(), &connectorbuilder.CredentialIssueInput{
			IdentityID:        &v2.ResourceId{ResourceType: userResourceType.Id, Resource: "u-1"},
			CredentialOptions: apiKeyIssueOptions(),
			RequestID:         "req-3",
		})
		require.Equal(t, codes.InvalidArgument, status.Code(err))
	})

	t.Run("unknown secret resource type", func(t *testing.T) {
		t.Parallel()
		_, err := newServiceAccountBuilder(fake.fakeCloudService).Issue(context.Background(), &connectorbuilder.CredentialIssueInput{
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

// TestDeleteAPIKeySkipsAKeyTheProviderAlreadyRetired keeps a revoke of an
// already-gone key idempotent without asking the provider to delete something it
// has already deleted: a provider that refuses that would turn a no-op revoke
// into a failure, contradicting the documented idempotency.
func TestDeleteAPIKeySkipsAKeyTheProviderAlreadyRetired(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		state resourcev1.ResourceState
	}{
		{name: "deleted", state: resourcev1.ResourceState_RESOURCE_STATE_DELETED},
		{name: "deleting", state: resourcev1.ResourceState_RESOURCE_STATE_DELETING},
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

			_, err := newAPIKeyBuilder(fake).Delete(context.Background(), &v2.ResourceId{ResourceType: apiKeyResourceType.Id, Resource: "key-1"}, nil)
			require.NoError(t, err)
		})
	}
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
