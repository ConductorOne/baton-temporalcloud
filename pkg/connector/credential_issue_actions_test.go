package connector

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"testing"
	"time"

	filippoage "filippo.io/age"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	configv1 "github.com/conductorone/baton-sdk/pb/c1/config/v1"
	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	"github.com/conductorone/baton-sdk/pkg/actions"
	"github.com/conductorone/baton-sdk/pkg/connectorbuilder"
	"github.com/conductorone/baton-temporalcloud/local_vendor/baton-multipass-sdk/pkg/multipass"

	"github.com/conductorone/baton-temporalcloud/pkg/client"

	cloudservicev1 "go.temporal.io/cloud-sdk/api/cloudservice/v1"
	identityv1 "go.temporal.io/cloud-sdk/api/identity/v1"
	operationv1 "go.temporal.io/cloud-sdk/api/operation/v1"
)

const (
	testOwnerID = "sa-1"
	testKeyID   = "key-1"
	testToken   = "vended-secret-value"
)

// issuerFake is a provider stub for the issuer with every answer defaulted to a
// successful, verified create. A test overrides only the answer it is about.
type issuerFake struct {
	*fakeCloudService

	// Provider answers a test may override.
	createErr   error
	noSecret    bool
	createOp    string // async operation id returned by the create
	opState     operationv1.AsyncOperation_State
	record      *identityv1.ApiKey // read-back override
	afterCreate func()

	created []*cloudservicev1.CreateApiKeyRequest
	deleted []*cloudservicev1.DeleteApiKeyRequest
}

func newIssuerFake(t *testing.T) *issuerFake {
	t.Helper()
	f := &issuerFake{opState: operationv1.AsyncOperation_STATE_FULFILLED}
	f.fakeCloudService = &fakeCloudService{
		createApiKey: func(_ context.Context, in *cloudservicev1.CreateApiKeyRequest) (*cloudservicev1.CreateApiKeyResponse, error) {
			require.Empty(t, in.GetAsyncOperationId(), "no request id exists, so none may be sent as an operation id")
			f.created = append(f.created, in)
			if f.afterCreate != nil {
				f.afterCreate()
			}
			if f.createErr != nil {
				return nil, f.createErr
			}
			token := testToken
			if f.noSecret {
				token = ""
			}
			resp := &cloudservicev1.CreateApiKeyResponse{KeyId: testKeyID, Token: token}
			if f.createOp != "" {
				resp.AsyncOperation = &operationv1.AsyncOperation{Id: f.createOp}
			}
			return resp, nil
		},
		getAsyncOp: func(_ context.Context, in *cloudservicev1.GetAsyncOperationRequest) (*cloudservicev1.GetAsyncOperationResponse, error) {
			return &cloudservicev1.GetAsyncOperationResponse{AsyncOperation: &operationv1.AsyncOperation{
				Id: in.GetAsyncOperationId(), State: f.opState, FailureReason: "provider refused",
			}}, nil
		},
		getApiKey: func(ctx context.Context, in *cloudservicev1.GetApiKeyRequest) (*cloudservicev1.GetApiKeyResponse, error) {
			if err := ctx.Err(); err != nil {
				return nil, status.FromContextError(err).Err()
			}
			if f.record != nil {
				return &cloudservicev1.GetApiKeyResponse{ApiKey: f.record}, nil
			}
			// The provider accepted the create, so the read-back echoes what was
			// asked for. A test that wants a disagreement sets f.record.
			return &cloudservicev1.GetApiKeyResponse{ApiKey: &identityv1.ApiKey{
				Id:              in.GetKeyId(),
				ResourceVersion: "v1",
				Spec:            proto.Clone(f.created[len(f.created)-1].GetSpec()).(*identityv1.ApiKeySpec),
			}}, nil
		},
		deleteApiKey: func(ctx context.Context, in *cloudservicev1.DeleteApiKeyRequest) (*cloudservicev1.DeleteApiKeyResponse, error) {
			// A delete on an already-canceled caller context never leaves the
			// process, so asserting here makes that regression fail loudly.
			require.NoError(t, ctx.Err(), "undo must not run on an already-canceled caller context")
			f.deleted = append(f.deleted, in)
			return &cloudservicev1.DeleteApiKeyResponse{}, nil
		},
	}
	return f
}

func (f *issuerFake) builder() *apiKeyBuilder {
	return newServiceAccountAPIKeyBuilder(f.fakeCloudService)
}

func (f *issuerFake) connector(t *testing.T) *Connector {
	t.Helper()
	c, err := client.New("test-api-key")
	require.NoError(t, err)
	c.CloudServiceClient = f.fakeCloudService
	return &Connector{cloudServiceClient: c}
}

func issueArgs(t *testing.T, days int) *structpb.Struct {
	t.Helper()
	return &structpb.Struct{Fields: map[string]*structpb.Value{
		actionArgServiceAccountID: subjectArg(t, serviceAccountResourceType.Id, testOwnerID),
		actionArgLifetimeDays:     structpb.NewNumberValue(float64(days)),
	}}
}

func subjectArg(t *testing.T, resourceType, resource string) *structpb.Value {
	t.Helper()
	v, err := structpb.NewValue(map[string]any{"resource_type_id": resourceType, "resource_id": resource})
	require.NoError(t, err)
	return v
}

// registeredIssuerSchemas runs the connector's syncers' ResourceActions into one
// ActionManager and returns the schemas registered for a resource type.
func registeredIssuerSchemas(t *testing.T, c *Connector, resourceTypeID string) []*v2.BatonActionSchema {
	t.Helper()
	ctx := context.Background()
	mgr := registerAllActions(t, c)
	schemas, _, err := mgr.ListActionSchemas(ctx, resourceTypeID)
	require.NoError(t, err)
	return schemas
}

func registerAllActions(t *testing.T, c *Connector) *actions.ActionManager {
	t.Helper()
	ctx := context.Background()
	mgr := actions.NewActionManager(ctx)
	for _, syncer := range c.ResourceSyncers(ctx) {
		provider, ok := syncer.(connectorbuilder.ResourceActionProvider)
		if !ok {
			continue
		}
		registry, err := mgr.GetTypeRegistry(ctx, syncer.ResourceType(ctx).GetId())
		require.NoError(t, err)
		require.NoError(t, provider.ResourceActions(ctx, registry))
	}
	return mgr
}

// 1. Schema shape: what C1 keys on to recognize a credential issuer.
func TestIssuerSchemaShape(t *testing.T) {
	t.Parallel()
	c := newIssuerFake(t).connector(t)

	schemas := registeredIssuerSchemas(t, c, serviceAccountAPIKeyResourceType.Id)
	require.Len(t, schemas, 1, "exactly one action on the secret type")
	schema := schemas[0]
	require.Equal(t, ActionIssueServiceAccountAPIKey, schema.GetName())
	require.Equal(t, serviceAccountAPIKeyResourceType.Id, schema.GetResourceTypeId())
	require.Equal(t, []v2.ActionType{v2.ActionType_ACTION_TYPE_RESOURCE_CREATE}, schema.GetActionType(), "a single RESOURCE_CREATE")

	var secrets, resources []*configv1.Field
	for _, f := range schema.GetReturnTypes() {
		if f.GetIsSecret() {
			secrets = append(secrets, f)
		}
		if f.HasResourceField() {
			resources = append(resources, f)
		}
	}
	require.Len(t, secrets, 1, "exactly one secret return")
	require.Equal(t, multipass.ContentType, secrets[0].GetContentType())
	require.Len(t, resources, 1, "exactly one resource return")
	require.Equal(t, actionReturnResource, resources[0].GetName())

	var subjects []*configv1.Field
	args := map[string]*configv1.Field{}
	for _, a := range schema.GetArguments() {
		args[a.GetName()] = a
		if a.HasResourceIdField() {
			subjects = append(subjects, a)
		}
	}
	require.Len(t, subjects, 1, "at most one subject")
	require.Equal(t, actionArgServiceAccountID, subjects[0].GetName())
	require.Equal(t, []string{serviceAccountResourceType.Id}, subjects[0].GetResourceIdField().GetRules().GetAllowedResourceTypeIds())

	// The lifetime is an admin-fixed offering value, not a subject.
	lifetime := args[actionArgLifetimeDays]
	require.NotNil(t, lifetime)
	require.True(t, lifetime.GetIsRequired())
	require.True(t, lifetime.HasIntField())
	require.Len(t, args, 2, "no other arguments, and no request or ticket id")

	require.Empty(t, registeredIssuerSchemas(t, c, userAPIKeyResourceType.Id), "user-owned keys have no issuer")
}

// 2. Handler result: the returned resource keeps its SecretTrait and the exact
// provider key id, and the plaintext is a valid api_key_v2.
func TestIssuerResult(t *testing.T) {
	t.Parallel()
	f := newIssuerFake(t)
	before := time.Now()

	response, plaintext, _, err := f.builder().issue(context.Background(), issueArgs(t, 30))
	require.NoError(t, err)

	res := response.GetFields()[actionReturnResource].GetStructValue()
	require.NotNil(t, res)
	id := res.GetFields()["resourceId"].GetStructValue().GetFields()
	require.Equal(t, serviceAccountAPIKeyResourceType.Id, id["resourceTypeId"].GetStringValue())
	require.Equal(t, testKeyID, id["resourceId"].GetStringValue(), "the exact provider key id, the one sync reports and delete accepts")
	require.Contains(t, res.GetFields()["annotations"].String(), "SecretTrait")
	require.NotContains(t, response.String(), testToken, "the structpb response never carries the key")

	require.Len(t, plaintext, 1)
	require.Equal(t, multipass.ContentType, plaintext[0].GetSchema())
	require.JSONEq(t, `{"key_value":"`+testToken+`","provider":"temporalcloud","key_id":"`+testKeyID+`"}`, string(plaintext[0].GetBytes()),
		"no nulls, optional fields omitted")

	require.Len(t, f.created, 1)
	spec := f.created[0].GetSpec()
	require.Equal(t, testOwnerID, spec.GetOwnerId())
	require.Equal(t, identityv1.OwnerType_OWNER_TYPE_SERVICE_ACCOUNT, spec.GetOwnerType())
	require.Regexp(t, `^c1-\d{8}T\d{6}Z-[a-z2-7]{6}$`, spec.GetDisplayName())
	want := before.Add(30 * 24 * time.Hour)
	require.WithinDuration(t, want, spec.GetExpiryTime().AsTime(), time.Minute, "the admin-fixed lifetime sets the provider expiry")
	require.Empty(t, f.deleted, "nothing is undone on success")
}

// SecretTrait.expires_at is the provider's recorded expiry, not the requested one.
func TestIssuerReportsTheProviderExpiry(t *testing.T) {
	t.Parallel()
	f := newIssuerFake(t)
	f.afterCreate = func() {
		approved := f.created[0].GetSpec().GetExpiryTime().AsTime()
		spec := proto.Clone(f.created[0].GetSpec()).(*identityv1.ApiKeySpec)
		spec.ExpiryTime = timestamppb.New(approved.Add(-30 * time.Second)) // inside the tolerance
		f.record = &identityv1.ApiKey{Id: testKeyID, ResourceVersion: "v1", Spec: spec}
	}

	response, _, _, err := f.builder().issue(context.Background(), issueArgs(t, 7))
	require.NoError(t, err)
	trait := secretTraitFromReturn(t, response)
	require.True(t, f.record.GetSpec().GetExpiryTime().AsTime().Equal(trait.GetExpiresAt().AsTime()))
}

// 3. Bad arguments are InvalidArgument and never reach the provider.
func TestIssuerRejectsBadArguments(t *testing.T) {
	t.Parallel()

	for name, args := range map[string]*structpb.Struct{
		"missing subject": {Fields: map[string]*structpb.Value{actionArgLifetimeDays: structpb.NewNumberValue(30)}},
		"wrong-type subject": {Fields: map[string]*structpb.Value{
			actionArgServiceAccountID: subjectArg(t, userResourceType.Id, "u-1"),
			actionArgLifetimeDays:     structpb.NewNumberValue(30),
		}},
		"missing lifetime":                   {Fields: map[string]*structpb.Value{actionArgServiceAccountID: subjectArg(t, serviceAccountResourceType.Id, testOwnerID)}},
		"zero lifetime":                      issueArgs(t, 0),
		"negative lifetime":                  issueArgs(t, -1),
		"lifetime past the provider maximum": issueArgs(t, apiKeyMaxLifetimeDays+1),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newIssuerFake(t)
			_, _, _, err := f.builder().issue(context.Background(), args)
			require.Equal(t, codes.InvalidArgument, status.Code(err))
			require.Empty(t, f.created, "a bad request must not create a key")
		})
	}
}

// 4. ActionManager round trip: the secret leaves only as ciphertext whose schema
// is the Multipass type.
func TestIssuerThroughActionManagerEncryptsTheSecret(t *testing.T) {
	ctx := context.Background()
	f := newIssuerFake(t)
	mgr := registerAllActions(t, f.connector(t))

	identity, err := filippoage.GenerateHybridIdentity()
	require.NoError(t, err)
	recipient := v2.EncryptionConfig_builder{
		AgeRecipientConfig: v2.EncryptionConfig_AgeRecipientConfig_builder{Recipient: identity.Recipient().String()}.Build(),
	}.Build()

	_, actionStatus, response, encrypted, _, err := mgr.InvokeActionWithWaitAndEncryption(
		ctx, ActionIssueServiceAccountAPIKey, serviceAccountAPIKeyResourceType.Id,
		issueArgs(t, 30), 5*time.Second, []*v2.EncryptionConfig{recipient})
	require.NoError(t, err)
	require.Equal(t, v2.BatonActionStatus_BATON_ACTION_STATUS_COMPLETE, actionStatus)

	require.Len(t, encrypted, 1)
	require.Equal(t, apiKeySecretFieldName, encrypted[0].GetName())
	require.Equal(t, multipass.ContentType, encrypted[0].GetSchema())
	require.NotContains(t, string(encrypted[0].GetEncryptedBytes()), testToken)
	require.NotContains(t, response.String(), testToken, "the public response never carries the key")

	reader, err := filippoage.Decrypt(bytes.NewReader(encrypted[0].GetEncryptedBytes()), identity)
	require.NoError(t, err)
	decrypted, err := io.ReadAll(reader)
	require.NoError(t, err)
	require.JSONEq(t, `{"key_value":"`+testToken+`","provider":"temporalcloud","key_id":"`+testKeyID+`"}`, string(decrypted))
}

// 5. Create then delete: deleting the returned resource id revokes that key.
func TestIssuedKeyIsRevokedByDeletingTheReturnedID(t *testing.T) {
	t.Parallel()
	f := newIssuerFake(t)
	b := f.builder()

	response, _, _, err := b.issue(context.Background(), issueArgs(t, 30))
	require.NoError(t, err)
	id := response.GetFields()[actionReturnResource].GetStructValue().GetFields()["resourceId"].GetStructValue().GetFields()
	returned := &v2.ResourceId{ResourceType: id["resourceTypeId"].GetStringValue(), Resource: id["resourceId"].GetStringValue()}
	require.Empty(t, f.deleted)

	_, err = b.Delete(context.Background(), returned, nil)
	require.NoError(t, err)
	require.Len(t, f.deleted, 1)
	require.Equal(t, testKeyID, f.deleted[0].GetKeyId(), "the provider is told to delete the same key the issuer created")
}

// Undo: every failure after the create deletes the key.
func TestIssuerUndoesTheCreateWhenTheResultCannotBeReturned(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		setup func(*issuerFake)
		code  codes.Code
	}{
		"no secret in the create response": {setup: func(f *issuerFake) { f.noSecret = true }, code: codes.Internal},
		"create operation failed": {setup: func(f *issuerFake) {
			f.createOp = "op-create"
			f.opState = operationv1.AsyncOperation_STATE_FAILED
		}, code: codes.Unknown},
		"read-back names another owner": {setup: func(f *issuerFake) {
			f.afterCreate = func() {
				spec := proto.Clone(f.created[0].GetSpec()).(*identityv1.ApiKeySpec)
				spec.OwnerId = "sa-other"
				f.record = &identityv1.ApiKey{Id: testKeyID, Spec: spec}
			}
		}, code: codes.Internal},
		"read-back expiry beyond the approved one": {setup: func(f *issuerFake) {
			f.afterCreate = func() {
				spec := proto.Clone(f.created[0].GetSpec()).(*identityv1.ApiKeySpec)
				spec.ExpiryTime = timestamppb.New(spec.GetExpiryTime().AsTime().Add(time.Hour))
				f.record = &identityv1.ApiKey{Id: testKeyID, Spec: spec}
			}
		}, code: codes.Internal},
		"read-back expiry far earlier than approved": {setup: func(f *issuerFake) {
			f.afterCreate = func() {
				spec := proto.Clone(f.created[0].GetSpec()).(*identityv1.ApiKeySpec)
				spec.ExpiryTime = timestamppb.New(spec.GetExpiryTime().AsTime().Add(-time.Hour))
				f.record = &identityv1.ApiKey{Id: testKeyID, Spec: spec}
			}
		}, code: codes.Internal},
		"read-back has no expiry": {setup: func(f *issuerFake) {
			f.afterCreate = func() {
				spec := proto.Clone(f.created[0].GetSpec()).(*identityv1.ApiKeySpec)
				spec.ExpiryTime = nil
				f.record = &identityv1.ApiKey{Id: testKeyID, Spec: spec}
			}
		}, code: codes.Internal},
		"read-back expiry has malformed nanos": {setup: func(f *issuerFake) {
			f.afterCreate = func() {
				spec := proto.Clone(f.created[0].GetSpec()).(*identityv1.ApiKeySpec)
				approved := spec.GetExpiryTime().AsTime()
				// Normalizes to exactly the approved instant; only CheckValid rejects it.
				spec.ExpiryTime = &timestamppb.Timestamp{Seconds: approved.Unix() - 1, Nanos: 1_000_000_000}
				f.record = &identityv1.ApiKey{Id: testKeyID, Spec: spec}
			}
		}, code: codes.Internal},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newIssuerFake(t)
			tc.setup(f)

			_, plaintext, _, err := f.builder().issue(context.Background(), issueArgs(t, 30))
			require.Error(t, err)
			require.Equal(t, tc.code, status.Code(err))
			require.Nil(t, plaintext)
			require.NotContains(t, err.Error(), testToken)
			require.Len(t, f.deleted, 1, "the key nobody will hold is deleted")
			require.Equal(t, testKeyID, f.deleted[0].GetKeyId())
		})
	}
}

// The undo must survive a canceled caller: the fake's delete fails the test if it
// is handed a canceled context.
func TestIssuerUndoSurvivesACanceledContext(t *testing.T) {
	t.Parallel()
	f := newIssuerFake(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.afterCreate = cancel // the request expires right after the create

	_, _, _, err := f.builder().issue(ctx, issueArgs(t, 30))
	require.Error(t, err)
	require.Len(t, f.deleted, 1, "the key is deleted even though the caller is gone")
}

// A failed undo is reported next to the original cause, never in place of it.
func TestIssuerReportsAFailedUndoWithoutReplacingTheCause(t *testing.T) {
	t.Parallel()
	f := newIssuerFake(t)
	f.noSecret = true
	f.deleteApiKey = func(context.Context, *cloudservicev1.DeleteApiKeyRequest) (*cloudservicev1.DeleteApiKeyResponse, error) {
		return nil, status.Error(codes.PermissionDenied, "not authorized")
	}

	_, _, _, err := f.builder().issue(context.Background(), issueArgs(t, 30))
	require.Equal(t, codes.Internal, status.Code(err), "the cause keeps its code")
	require.Contains(t, err.Error(), "may remain at the provider")
	require.Contains(t, err.Error(), testKeyID)
}

func TestIssuerSurfacesCreateFailureWithoutUndo(t *testing.T) {
	t.Parallel()
	f := newIssuerFake(t)
	f.createErr = status.Error(codes.ResourceExhausted, "limit")

	_, _, _, err := f.builder().issue(context.Background(), issueArgs(t, 30))
	require.Equal(t, codes.ResourceExhausted, status.Code(err))
	require.Empty(t, f.deleted, "nothing was created, so nothing is undone")
}

// Two calls make two keys with different names: no request id is consulted.
func TestIssuerCallsAreIndependent(t *testing.T) {
	t.Parallel()
	f := newIssuerFake(t)
	for range 2 {
		_, _, _, err := f.builder().issue(context.Background(), issueArgs(t, 30))
		require.NoError(t, err)
	}
	require.Len(t, f.created, 2)
	require.NotEqual(t, f.created[0].GetSpec().GetDisplayName(), f.created[1].GetSpec().GetDisplayName())
}

func TestNewIssuedKeyName(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 1, 2, 15, 4, 5, 0, time.FixedZone("x", 3600))
	name, err := newIssuedKeyName(now, bytes.NewReader([]byte{0, 0, 0, 0}))
	require.NoError(t, err)
	require.Equal(t, "c1-20260102T140405Z-aaaaaa", name, "UTC timestamp, base32 suffix")

	again, err := newIssuedKeyName(now, rand.Reader)
	require.NoError(t, err)
	require.Regexp(t, `^c1-\d{8}T\d{6}Z-[a-z2-7]{6}$`, again)

	_, err = newIssuedKeyName(now, bytes.NewReader(nil))
	require.Error(t, err)
}

func secretTraitFromReturn(t *testing.T, response *structpb.Struct) *v2.SecretTrait {
	t.Helper()
	// Rebuild the resource from the structpb return the way C1 would read it.
	res := response.GetFields()[actionReturnResource].GetStructValue()
	require.NotNil(t, res)
	basic, ok := actions.GetResourceFieldArg(response, actionReturnResource)
	require.True(t, ok)
	return secretTrait(t, basic)
}
