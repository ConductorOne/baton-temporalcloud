package connector

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	"github.com/conductorone/baton-sdk/pkg/annotations"
	"github.com/conductorone/baton-sdk/pkg/connectorbuilder"
	"github.com/conductorone/baton-sdk/pkg/crypto/providers"
	"github.com/conductorone/baton-sdk/pkg/crypto/providers/jwk"
	rs "github.com/conductorone/baton-sdk/pkg/types/resource"
	"github.com/go-jose/go-jose/v4"

	"github.com/conductorone/baton-temporalcloud/pkg/client"
)

// This file pins what the credential-issuance path gets from mechanisms that
// already exist in the SDK, so a future typed-output contract is not adopted on
// the assumption that these cannot express the requirement. Each test drives
// the real SDK entry point rather than the connector's own function, because
// the SDK is what enforces the contract.
//
// The three mechanisms under test:
//
//  1. Pre-mint refusal. `resolveCredentialIssueDescriptor` matches the request's
//     option arm together with `secret_resource_type_id` against the connector's
//     advertised descriptors, and the builder refuses an unadvertised pair
//     before the connector is invoked. That is a pre-dispatch declaration: the
//     pair is named in the request, not inferred from a response.
//  2. Arbitrary bytes. A connector's `PlaintextData` is encrypted as-is, and the
//     encryptors carry `Name`/`Description`/`Schema` through to `EncryptedData`.
//     An API key, an opaque JSON bundle, and a PEM blob all ride the same path.
//  3. Resource output type. The resource the connector returns must carry the
//     `secret_resource_type_id` the descriptor advertised, or the builder
//     rejects the output. The typed identity is therefore bound to a
//     declaration the SDK already checks, not to a response field.

// contractServer builds the real Temporal Cloud connector behind the SDK's own
// builder, so every call below goes through the production validation path.
func contractServer(t *testing.T, fake *issuanceFake) interface {
	IssueCredential(context.Context, *v2.IssueCredentialRequest) (*v2.IssueCredentialResponse, error)
} {
	t.Helper()
	c := &Connector{cloudServiceClient: clientFromFake(t, fake)}
	server, err := connectorbuilder.NewConnector(context.Background(), c)
	require.NoError(t, err)
	return server.(interface {
		IssueCredential(context.Context, *v2.IssueCredentialRequest) (*v2.IssueCredentialResponse, error)
	})
}

// clientFromFake adapts the fake provider client to the concrete client type
// the Connector holds. It is a thin wrapper because the connector's field is
// the concrete *client.Client; the RPCs still route to the fake.
func clientFromFake(t *testing.T, fake *issuanceFake) *client.Client {
	t.Helper()
	c, err := client.New("test-api-key")
	require.NoError(t, err)
	c.CloudServiceClient = fake.fakeCloudService
	return c
}

func encryptionKey(t *testing.T) (*v2.EncryptionConfig, *jose.JSONWebKey) {
	t.Helper()
	config, privateKey, err := (&jwk.JWKEncryptionProvider{}).GenerateKey(context.Background())
	require.NoError(t, err)
	return config, privateKey
}

func issueRequest(identity *v2.ResourceId, options *v2.CredentialIssueOptions, requestID string, configs []*v2.EncryptionConfig) *v2.IssueCredentialRequest {
	return v2.IssueCredentialRequest_builder{
		IdentityId:        identity,
		CredentialOptions: options,
		RequestId:         requestID,
		EncryptionConfigs: configs,
	}.Build()
}

// TestIssuanceRefusesUnadvertisedPairBeforeMinting is the pre-mint refusal: the
// request names a secret resource type the connector never advertised, and the
// builder rejects it before the connector is invoked at all.
func TestIssuanceRefusesUnadvertisedPairBeforeMinting(t *testing.T) {
	t.Parallel()

	fake := newIssuanceFake(t, "sa-1", time.Now().Add(time.Hour))
	server := contractServer(t, fake)
	config, _ := encryptionKey(t)

	_, err := server.IssueCredential(context.Background(), issueRequest(
		&v2.ResourceId{ResourceType: serviceAccountResourceType.Id, Resource: "sa-1"},
		v2.CredentialIssueOptions_builder{
			SecretResourceTypeId: "some-other-secret-type",
			ApiKey:               v2.CredentialIssueOptions_ApiKey_builder{}.Build(),
		}.Build(),
		"req-unadvertised",
		[]*v2.EncryptionConfig{config},
	))
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.ErrorContains(t, err, "does not produce secret resource type")
	require.Empty(t, fake.created,
		"an unadvertised pair must be refused before the connector reaches the provider")
}

// TestIssuanceRefusesUnadvertisedOptionShape proves the option arm is part of the
// match, not just the resource type: a caller cannot obtain the api-key
// descriptor by asking for a different credential shape.
func TestIssuanceRefusesUnadvertisedOptionShape(t *testing.T) {
	t.Parallel()

	fake := newIssuanceFake(t, "sa-1", time.Now().Add(time.Hour))
	server := contractServer(t, fake)
	config, _ := encryptionKey(t)

	_, err := server.IssueCredential(context.Background(), issueRequest(
		&v2.ResourceId{ResourceType: serviceAccountResourceType.Id, Resource: "sa-1"},
		v2.CredentialIssueOptions_builder{
			SecretResourceTypeId: apiKeyResourceType.Id,
			Token:                v2.CredentialIssueOptions_Token_builder{}.Build(),
		}.Build(),
		"req-wrong-shape",
		[]*v2.EncryptionConfig{config},
	))
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Empty(t, fake.created)
}

// TestIssuanceCarriesArbitraryBytesThroughEncryption is the delivery proof: the
// connector's plaintext reaches the recipient byte-for-byte, with its field name
// intact, through the SDK's existing encryption path and no typed contract.
func TestIssuanceCarriesArbitraryBytesThroughEncryption(t *testing.T) {
	t.Parallel()

	// A value that is not a bare token: opaque JSON with characters an
	// over-eager normalizer would mangle. The path must not care.
	const opaque = `{"key":"abc-123","note":"padded  spaces","nested":{"n":1}}`
	fake := newIssuanceFake(t, "sa-1", time.Now().Add(time.Hour))
	fake.token = opaque
	server := contractServer(t, fake)
	config, privateKey := encryptionKey(t)

	identity := &v2.ResourceId{ResourceType: serviceAccountResourceType.Id, Resource: "sa-1"}
	resp, err := server.IssueCredential(context.Background(), issueRequest(
		identity, apiKeyIssueOptions(), "req-bytes", []*v2.EncryptionConfig{config},
	))
	require.NoError(t, err)
	require.Len(t, resp.GetEncryptedData(), 1)
	require.Equal(t, "api_key", resp.GetEncryptedData()[0].GetName(),
		"the connector's field name must survive encryption")
	require.Equal(t, apiKeyResourceType.Id, resp.GetSecret().GetId().GetResourceType(),
		"the issued resource must carry the advertised secret resource type")
	require.Equal(t, "key-1", resp.GetSecret().GetId().GetResource())

	// The recipient side: decrypt with the private key and compare bytes.
	decryptor, err := providers.GetDecryptionProviderForConfig(context.Background(), &providers.DecryptionConfig{
		Provider:   jwk.EncryptionProviderJwk,
		PrivateKey: privateKey,
	})
	require.NoError(t, err)
	plaintext, err := decryptor.Decrypt(context.Background(), resp.GetEncryptedData()[0], privateKey)
	require.NoError(t, err)
	require.Equal(t, opaque, string(plaintext.GetBytes()),
		"the recipient must receive exactly the bytes the connector produced")
	require.Equal(t, "api_key", plaintext.GetName())
}

// TestIssuanceBindsTheReturnedResourceTypeToTheDeclaration proves the typed
// identity is a resource output type the SDK already checks: a connector whose
// secret resource does not carry the advertised type is rejected, so the
// declared `secret_resource_type_id` cannot silently diverge from what was
// actually minted.
func TestIssuanceBindsTheReturnedResourceTypeToTheDeclaration(t *testing.T) {
	t.Parallel()

	config, _ := encryptionKey(t)
	server, issued := stubIssuerServer(t, "different-secret-type")

	_, err := server.IssueCredential(context.Background(), issueRequest(
		&v2.ResourceId{ResourceType: stubIdentityType.Id, Resource: "identity-1"},
		v2.CredentialIssueOptions_builder{
			SecretResourceTypeId: "advertised-secret-type",
			ApiKey:               v2.CredentialIssueOptions_ApiKey_builder{}.Build(),
		}.Build(),
		"req-mismatch",
		[]*v2.EncryptionConfig{config},
	))
	require.Equal(t, codes.Internal, status.Code(err))
	require.ErrorContains(t, err, "secret resource type does not match advertised capability")
	require.True(t, *issued, "the connector ran; the SDK rejected its output")
}

// TestIssuanceAcceptsMatchingResourceType is the control: the same stub with a
// matching type is accepted, so the rejection above is the type binding and not
// a malformed stub.
func TestIssuanceAcceptsMatchingResourceType(t *testing.T) {
	t.Parallel()

	config, _ := encryptionKey(t)
	server, issued := stubIssuerServer(t, "advertised-secret-type")

	resp, err := server.IssueCredential(context.Background(), issueRequest(
		&v2.ResourceId{ResourceType: stubIdentityType.Id, Resource: "identity-1"},
		v2.CredentialIssueOptions_builder{
			SecretResourceTypeId: "advertised-secret-type",
			ApiKey:               v2.CredentialIssueOptions_ApiKey_builder{}.Build(),
		}.Build(),
		"req-match",
		[]*v2.EncryptionConfig{config},
	))
	require.NoError(t, err)
	require.True(t, *issued)
	require.Equal(t, "advertised-secret-type", resp.GetSecret().GetId().GetResourceType())
}

// --- stub connector for the resource-output-type tests ----------------------

var stubIdentityType = &v2.ResourceType{Id: "stub-identity", DisplayName: "Stub Identity"}

type stubIssuerBuilder struct {
	secretResourceType string
	issued             *bool
}

func (stubIssuerBuilder) Metadata(context.Context) (*v2.ConnectorMetadata, error) {
	return &v2.ConnectorMetadata{DisplayName: "contract stub"}, nil
}

func (stubIssuerBuilder) Validate(context.Context) (annotations.Annotations, error) { return nil, nil }

func (b stubIssuerBuilder) ResourceSyncers(context.Context) []connectorbuilder.ResourceSyncerV2 {
	return []connectorbuilder.ResourceSyncerV2{
		&stubIssuer{secretResourceType: b.secretResourceType, issued: b.issued},
		&stubSecretType{id: "advertised-secret-type"},
		&stubSecretType{id: "different-secret-type"},
	}
}

type stubIssuer struct {
	secretResourceType string
	issued             *bool
}

func (s *stubIssuer) ResourceType(context.Context) *v2.ResourceType { return stubIdentityType }

func (s *stubIssuer) List(context.Context, *v2.ResourceId, rs.SyncOpAttrs) ([]*v2.Resource, *rs.SyncOpResults, error) {
	return nil, nil, nil
}

func (s *stubIssuer) Entitlements(context.Context, *v2.Resource, rs.SyncOpAttrs) ([]*v2.Entitlement, *rs.SyncOpResults, error) {
	return nil, nil, nil
}

func (s *stubIssuer) Grants(context.Context, *v2.Resource, rs.SyncOpAttrs) ([]*v2.Grant, *rs.SyncOpResults, error) {
	return nil, nil, nil
}

func (s *stubIssuer) IssueCapabilityDetails(context.Context) (*v2.CredentialDetailsCredentialIssue, annotations.Annotations, error) {
	return v2.CredentialDetailsCredentialIssue_builder{
		Options: []*v2.CredentialIssueOptionDescriptor{
			v2.CredentialIssueOptionDescriptor_builder{
				Option:               v2.CapabilityDetailCredentialOption_CAPABILITY_DETAIL_CREDENTIAL_OPTION_API_KEY,
				ResourceMode:         v2.CredentialResourceMode_CREDENTIAL_RESOURCE_MODE_DISCOVERABLE,
				SecretResourceTypeId: "advertised-secret-type",
			}.Build(),
		},
		PreferredOption: v2.CapabilityDetailCredentialOption_CAPABILITY_DETAIL_CREDENTIAL_OPTION_API_KEY,
	}.Build(), nil, nil
}

func (s *stubIssuer) Issue(_ context.Context, input *connectorbuilder.CredentialIssueInput) (*connectorbuilder.CredentialIssueOutput, error) {
	*s.issued = true
	secret, err := rs.NewSecretResource("stub", &v2.ResourceType{Id: s.secretResourceType, DisplayName: "Stub Secret"}, "secret-1",
		[]rs.SecretTraitOption{
			rs.WithSecretIdentityID(input.IdentityID),
			rs.WithSecretType(v2.SecretTrait_CREDENTIAL_TYPE_STATIC_SECRET),
		})
	if err != nil {
		return nil, err
	}
	return &connectorbuilder.CredentialIssueOutput{
		Secret:        secret,
		PlaintextData: []*v2.PlaintextData{v2.PlaintextData_builder{Name: "api_key", Bytes: []byte("stub-secret")}.Build()},
		ResourceMode:  v2.CredentialResourceMode_CREDENTIAL_RESOURCE_MODE_DISCOVERABLE,
	}, nil
}

type stubSecretType struct{ id string }

func (s *stubSecretType) ResourceType(context.Context) *v2.ResourceType {
	return &v2.ResourceType{Id: s.id, DisplayName: s.id, Traits: []v2.ResourceType_Trait{v2.ResourceType_TRAIT_SECRET}}
}

func (s *stubSecretType) List(context.Context, *v2.ResourceId, rs.SyncOpAttrs) ([]*v2.Resource, *rs.SyncOpResults, error) {
	return nil, nil, nil
}

func (s *stubSecretType) Entitlements(context.Context, *v2.Resource, rs.SyncOpAttrs) ([]*v2.Entitlement, *rs.SyncOpResults, error) {
	return nil, nil, nil
}

func (s *stubSecretType) Grants(context.Context, *v2.Resource, rs.SyncOpAttrs) ([]*v2.Grant, *rs.SyncOpResults, error) {
	return nil, nil, nil
}

func (s *stubSecretType) Delete(context.Context, *v2.ResourceId, *v2.ResourceId) (annotations.Annotations, error) {
	return nil, nil
}

func stubIssuerServer(t *testing.T, secretResourceType string) (interface {
	IssueCredential(context.Context, *v2.IssueCredentialRequest) (*v2.IssueCredentialResponse, error)
}, *bool) {
	t.Helper()
	issued := new(bool)
	server, err := connectorbuilder.NewConnector(context.Background(), stubIssuerBuilder{
		secretResourceType: secretResourceType,
		issued:             issued,
	})
	require.NoError(t, err)
	return server.(interface {
		IssueCredential(context.Context, *v2.IssueCredentialRequest) (*v2.IssueCredentialResponse, error)
	}), issued
}
