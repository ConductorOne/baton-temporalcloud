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

// IssueCapabilityDetails advertises the credential kind this connector mints:
// an API key owned by the service account the request names.
//
// The plaintext value it produces is the canonical `api_key_v2` document, the
// content type the shipped Multipass catalog declares for an API key or token.
// It is produced under this one selector rather than under a second one because
// there is no earlier contract to preserve: no released version of this
// connector has ever carried a credential-issue capability at all -- every tag
// through v0.1.3 lacks it -- so the first shape that ships can simply be the
// correct one. A second option arm or a second secret resource type would exist
// only to fence a cohort that does not exist.
//
// It is DISCOVERABLE because Temporal Cloud can enumerate API keys, and the
// api-key resource type above is declared and synced so an issued key appears
// in subsequent syncs instead of only in the issuance response.
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

// apiKeyVendedValue renders one issued key as the canonical `api_key_v2`
// plaintext document, and names it.
//
// The name is a label, not a declaration. The SDK carries it beside the
// encrypted bytes, and nothing may infer a content type from it: the type is
// bound on C1's side before the provider is called, and a response field cannot
// forge a pre-dispatch binding. The name matches the content type only so an
// operator reading a run sees the same word in both places.
func apiKeyVendedValue(token, keyID string) ([]byte, string, error) {
	value, err := apiKeyV2Value(token, keyID)
	if err != nil {
		return nil, "", fmt.Errorf("baton-temporalcloud: encode %s document: %w", apiKeyV2ContentType, err)
	}
	return value, apiKeyV2ContentType, nil
}

// Issue mints one Temporal Cloud API key owned by the requested service
// account. It never creates or modifies an identity: the owner must already
// exist and be synced, and the key inherits the owner's permissions.
//
// # Request identity
//
// Every provider request this connector makes carries a deterministic async
// operation id derived from the C1 request id. Temporal Cloud documents that
// field only as "the id to use for this async operation"; it documents no
// replay or deduplication on it. The connector therefore treats it as a request
// identity it can OBSERVE after the fact rather than as a guarantee. Before
// creating anything it asks the provider whether that operation already exists,
// as a SECOND signal alongside the key listing: the operation record is a
// different read, so it catches a create whose response was lost in cases the
// listing would miss. It is not claimed to be immediate -- a provider that has
// not yet recorded the operation answers like a first attempt, and the by-name
// lookup remains the primary guard. Whether two concurrent creates with the
// same operation id collapse into one key is the provider's contract, not this
// connector's: the connector supplies the identity and does not rely on it.
//
// # Why a retry cannot re-deliver
//
// Temporal Cloud returns a key's secret exactly once, at creation, and its API
// exposes no way to read it back. An existing key for this request identity can
// therefore never be handed to the caller. The connector refuses rather than
// minting a second key, and it does NOT delete the existing one: it cannot
// prove the earlier attempt's secret was never delivered, and deleting a
// delivered credential would be a silent revocation. The error names the key so
// an operator can revoke it and retry.
//
// # Why the create is not awaited
//
// The response already carries the key id and secret, and a create whose wait
// timed out would leave a key this call can never return. Returning the
// provider's own answer keeps "created" and "returned" the same event.
//
// # Interleavings, and the conservative outcome for each
//
// The rule behind every row: fail closed whenever the provider gives no verdict,
// and never delete a credential this connector cannot prove was undelivered. A
// stranded key is visible and revocable; a duplicate credential is silent, and
// deleting a delivered one is a silent revocation.
//
//   - Clean attempt: creates, verifies the provider's record, returns. Exactly
//     one provider credential.
//   - Retry after a clean success: the by-name pre-check finds the key and
//     refuses with AlreadyExists, naming it. Nothing is deleted. Still one.
//   - Create committed, response lost: the attempt returns the transport error;
//     the retry finds an operation for this request identity and refuses,
//     naming the key when the listing shows it. One credential exists, but its
//     secret was never delivered, so it is unusable; the error tells an operator
//     to revoke it and re-request.
//   - Create committed, listing lagging: when the operation record is visible
//     and the key listing is not, the operation read-back still refuses, so no
//     duplicate is minted. This is what the second read buys. It is not a claim
//     that the operation record is always ahead of the listing.
//   - Operation record or key listing unreadable: fails closed with the
//     provider's own code. Nothing is minted. Unchanged.
//   - Create fails unambiguously: the provider's error is returned, augmented
//     for the key limit. Nothing was created. Whether this request id may be
//     used again depends on whether the provider recorded an operation for the
//     attempt: if it did, the id is spent and a new request is needed.
//   - Operation FAILED or CANCELLED: refuses. An earlier attempt existed, and
//     its state is not proof that the provider created no key -- a create can
//     commit and then report a failure -- so the key listing is not consulted
//     for permission either. The request id is spent; a caller must raise a new
//     request.
//   - Read-back disagrees with the request, or the response carries no secret:
//     the key is rolled back and the failure is returned. Zero, or one the error
//     names as possibly remaining when cleanup itself failed.
//   - Two attempts race past the pre-checks: both send the same request identity
//     and the same display name, so the provider has what it needs to collapse
//     them; whether it does is its contract, and Temporal Cloud documents none.
//     The connector deletes neither key. This is the one row that can leave two
//     provider credentials, and it is recorded rather than papered over. A
//     serial retry after an unknown create can duplicate the same way when
//     neither read shows the key; C1's single-dispatch and no-redrive rule
//     contains its own path, not every direct SDK caller.
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
	opID := apiKeyOperationID(input.RequestID)

	// A second, independent read alongside the key listing: the provider's own
	// record that an attempt for this request identity already happened. Only
	// NotFound permits minting; see the function for why every other state
	// refuses.
	attempted, err := o.apiKeyRequestAlreadyAttempted(ctx, opID)
	if err != nil {
		return nil, err
	}
	if attempted {
		existing, lookupErr := o.findAPIKeyByName(ctx, ownerID, name)
		if lookupErr != nil {
			return nil, lookupErr
		}
		return nil, apiKeyAlreadyIssuedError(input.RequestID, ownerID, existing)
	}

	existing, err := o.findAPIKeyByName(ctx, ownerID, name)
	if err != nil {
		return nil, err
	}
	if existing != "" {
		return nil, apiKeyAlreadyIssuedError(input.RequestID, ownerID, existing)
	}

	resp, err := o.client.CreateApiKey(ctx, &cloudservicev1.CreateApiKeyRequest{
		Spec: &identityv1.ApiKeySpec{
			OwnerId:     ownerID,
			OwnerType:   identityv1.OwnerType_OWNER_TYPE_SERVICE_ACCOUNT,
			DisplayName: name,
			Description: fmt.Sprintf("ConductorOne vended credential for request %s", input.RequestID),
			ExpiryTime:  timestamppb.New(expiresAt),
		},
		AsyncOperationId: opID,
	})
	if err != nil {
		return nil, apiKeyCreateError(err)
	}
	keyID := resp.GetKeyId()
	if keyID == "" {
		return nil, status.Error(codes.Internal, "baton-temporalcloud: provider returned an API key with no id")
	}
	if resp.GetToken() == "" {
		return nil, o.rollbackCreatedAPIKey(ctx, keyID, ownerID,
			status.Error(codes.Internal, "baton-temporalcloud: provider returned an API key with no secret"))
	}

	// The provider's own record is the authority for owner and expiry. What
	// this connector asked for is not evidence of what the provider stored, so
	// the returned instant is the one the provider reported.
	actualExpiry, err := o.verifyIssuedAPIKey(ctx, keyID, ownerID, expiresAt)
	if err != nil {
		return nil, o.rollbackCreatedAPIKey(ctx, keyID, ownerID, err)
	}

	secret, err := rs.NewSecretResource(name, apiKeyResourceType, keyID, []rs.SecretTraitOption{
		// The SDK requires the secret trait's identity to equal the
		// authenticating identity, so this is the requested service account,
		// not whoever happens to be the connector's principal.
		rs.WithSecretIdentityID(input.IdentityID),
		rs.WithSecretType(v2.SecretTrait_CREDENTIAL_TYPE_STATIC_SECRET),
		rs.WithSecretDetail(apiKeyCredentialDetail),
		rs.WithSecretExpiresAt(actualExpiry),
	}, rs.WithParentResourceID(input.IdentityID), rs.WithResourceCreatedAt(now))
	if err != nil {
		return nil, o.rollbackCreatedAPIKey(ctx, keyID, ownerID,
			fmt.Errorf("baton-temporalcloud: build API key secret resource: %w", err))
	}

	value, plaintextName, err := apiKeyVendedValue(resp.GetToken(), keyID)
	if err != nil {
		// The credential is minted but cannot be rendered in the shape its
		// selector promised, so it can never be delivered. Roll it back rather
		// than hand C1 bytes that do not match what the caller named.
		return nil, o.rollbackCreatedAPIKey(ctx, keyID, ownerID, err)
	}

	return &connectorbuilder.CredentialIssueOutput{
		Secret: secret,
		PlaintextData: []*v2.PlaintextData{
			v2.PlaintextData_builder{Name: plaintextName, Bytes: value}.Build(),
		},
		ResourceMode: v2.CredentialResourceMode_CREDENTIAL_RESOURCE_MODE_DISCOVERABLE,
	}, nil
}

// apiKeyRequestAlreadyAttempted reports whether the provider already holds an
// operation for this request identity.
//
// NotFound is the expected answer for a first attempt, and it is the ONLY
// answer that permits minting. Every other outcome refuses:
//
//   - FULFILLED: an earlier attempt's create committed.
//   - PENDING or IN_PROGRESS: another attempt for this request is in flight.
//   - FAILED or CANCELLED: an earlier attempt existed, and its state does NOT
//     prove the provider created no key -- a create can commit and then report
//     a failure. An empty key listing does not supply that missing proof
//     either, because the listing can lag.
//   - UNSPECIFIED, or a state this connector does not recognise: no verdict.
//   - A response with no operation at all: also no verdict.
//
// Refusing on a failed attempt costs a stranded request id: a request whose
// first attempt genuinely failed cannot be re-driven under the same id, so a
// caller must raise a new request. That is the conservative direction, and it
// is a deliberate trade -- a duplicate credential is silent, whereas a stuck
// request is visible and its error names the key and the recovery.
func (o *serviceAccountBuilder) apiKeyRequestAlreadyAttempted(ctx context.Context, opID string) (bool, error) {
	if _, err := o.client.GetAsyncOperation(ctx, &cloudservicev1.GetAsyncOperationRequest{AsyncOperationId: opID}); err != nil {
		if status.Code(err) == codes.NotFound {
			return false, nil
		}
		return false, fmt.Errorf("baton-temporalcloud: failed to read async operation %q: %w", opID, err)
	}
	// The provider answered, so an operation for this request identity exists.
	// Its state is not consulted: none of the states is proof that no key was
	// created.
	return true, nil
}

// verifyIssuedAPIKey confirms from the provider's own record that the key it
// created belongs to the requested service account and does not outlive the
// approved expiry. It returns the expiry the provider actually recorded.
//
// The comparison is deliberately one-sided. A provider expiry LATER than the
// approved deadline is rejected outright, with no tolerance: the credential
// would outlive the lifetime that was approved, which is the one direction that
// matters. An expiry EARLIER than requested is accepted within
// apiKeyExpiryReadbackTolerance, because the provider rounds the instant it
// stores and a shorter-lived credential is not a safety problem.
//
// The returned instant is the provider's, not the requested one. Returning the
// requested value would report an expiry the provider never confirmed, which is
// the same class of error as accepting a timestamp as evidence of a stored one.
func (o *serviceAccountBuilder) verifyIssuedAPIKey(ctx context.Context, keyID, ownerID string, approvedExpiry time.Time) (time.Time, error) {
	resp, err := o.getIssuedAPIKey(ctx, keyID)
	if err != nil {
		return time.Time{}, err
	}
	spec := resp.GetApiKey().GetSpec()
	if spec.GetOwnerId() != ownerID {
		return time.Time{}, status.Errorf(codes.Internal,
			"baton-temporalcloud: provider recorded API key %q against owner %q, not the requested %q", keyID, spec.GetOwnerId(), ownerID)
	}
	if spec.GetOwnerType() != identityv1.OwnerType_OWNER_TYPE_SERVICE_ACCOUNT {
		return time.Time{}, status.Errorf(codes.Internal,
			"baton-temporalcloud: provider recorded API key %q against owner type %s, not a service account", keyID, spec.GetOwnerType())
	}
	providerExpiry := spec.GetExpiryTime()
	if providerExpiry == nil {
		return time.Time{}, status.Errorf(codes.Internal, "baton-temporalcloud: provider recorded API key %q with no expiry", keyID)
	}
	// AsTime normalizes malformed nanos instead of reporting them, so an
	// invalid timestamp has to be rejected before it is treated as the
	// provider's authoritative instant.
	if err := providerExpiry.CheckValid(); err != nil {
		return time.Time{}, status.Errorf(codes.Internal,
			"baton-temporalcloud: provider recorded API key %q with an invalid expiry timestamp: %v", keyID, err)
	}
	actual := providerExpiry.AsTime()
	if actual.After(approvedExpiry) {
		return time.Time{}, status.Errorf(codes.Internal,
			"baton-temporalcloud: provider recorded API key %q expiring at %s, beyond the approved %s",
			keyID, actual.UTC(), approvedExpiry.UTC())
	}
	if approvedExpiry.Sub(actual) > apiKeyExpiryReadbackTolerance {
		return time.Time{}, status.Errorf(codes.Internal,
			"baton-temporalcloud: provider recorded API key %q expiring at %s, materially earlier than the requested %s",
			keyID, actual.UTC(), approvedExpiry.UTC())
	}
	return actual, nil
}

// getIssuedAPIKey reads back a key this call just created, retrying NotFound
// over a short bounded window.
//
// Only NotFound is retried, and only here. The create is deliberately not
// awaited, so this read can race the provider's own write and see a key that
// the provider has already returned an id for. Treating that as a failure would
// roll the key back -- destroying a valid credential, spending the request id,
// and, if the rollback's delete also raced the write, leaving the key to appear
// later as an untracked credential. Any other error is the provider's answer
// and is returned immediately, so a permission or transport failure is not
// mistaken for a slow write.
func (o *serviceAccountBuilder) getIssuedAPIKey(ctx context.Context, keyID string) (*cloudservicev1.GetApiKeyResponse, error) {
	var lastErr error
	for attempt := 0; attempt < apiKeyReadbackAttempts; attempt++ {
		resp, err := o.client.GetApiKey(ctx, &cloudservicev1.GetApiKeyRequest{KeyId: keyID})
		if err == nil {
			return resp, nil
		}
		if status.Code(err) != codes.NotFound {
			return nil, fmt.Errorf("baton-temporalcloud: failed to read back issued API key %q: %w", keyID, err)
		}
		lastErr = err
		if attempt == apiKeyReadbackAttempts-1 {
			break
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("baton-temporalcloud: reading back issued API key %q: %w", keyID, ctx.Err())
		case <-time.After(apiKeyReadbackInterval):
		}
	}
	return nil, fmt.Errorf("baton-temporalcloud: failed to read back issued API key %q after %d attempts: %w",
		keyID, apiKeyReadbackAttempts, lastErr)
}

// rollbackCreatedAPIKey deletes a key this call created but cannot deliver, and
// returns cause unchanged when cleanup succeeds.
//
// A cleanup failure never replaces the original cause: the caller must still
// learn why issuance failed. It is appended instead, so an operator is also
// told that a key may remain at the provider.
//
// The whole cleanup -- the delete and the wait for its asynchronous result --
// runs on one bounded context derived with context.WithoutCancel, because
// cleanup must not be abandoned by the request that triggered it. The caller's
// context is typically already cancelled or past its deadline at this point (a
// read-back that failed because the request expired is the common way to get
// here), so passing it to the delete would mean the delete never leaves the
// process and the key is silently left behind.
func (o *serviceAccountBuilder) rollbackCreatedAPIKey(ctx context.Context, keyID, ownerID string, cause error) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), apiKeyDeletionMaxDuration)
	defer cancel()

	resp, err := o.client.DeleteApiKey(cleanupCtx, &cloudservicev1.DeleteApiKeyRequest{KeyId: keyID})
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return cause
		}
		ctxzap.Extract(ctx).Warn(
			"baton-temporalcloud: failed to clean up API key after issuance failure",
			zap.String("api_key_id", keyID),
			zap.String("service_account_id", ownerID),
			zap.Error(err),
		)
		// Only the cause is wrapped: the cleanup failure is rendered as text so
		// that it cannot take over the error's gRPC code. status.Code resolves
		// the first wrapped error that carries one, and the caller must keep
		// seeing why issuance failed, not why its cleanup did.
		return fmt.Errorf("%w (cleanup failed: API key %s for service account %s may remain at the provider: %s)", cause, keyID, ownerID, err.Error())
	}
	opID := resp.GetAsyncOperation().GetId()
	if opID == "" {
		return cause
	}
	l := ctxzap.Extract(ctx).With(zap.String("request_id", opID), zap.String("api_key_id", keyID))
	retryDelay := asyncCheckDelay(resp.GetAsyncOperation().GetCheckDuration().AsDuration())
	if waitErr := awaitAsyncOperation(cleanupCtx, l, o.client, opID, retryDelay); waitErr != nil {
		ctxzap.Extract(ctx).Warn(
			"baton-temporalcloud: API key cleanup did not complete",
			zap.String("api_key_id", keyID),
			zap.Error(waitErr),
		)
		// Same reason as the delete failure above: the cause stays the only
		// wrapped error, so the cleanup outcome cannot replace its code.
		return fmt.Errorf("%w (cleanup incomplete: API key %s for service account %s may remain at the provider: %s)", cause, keyID, ownerID, waitErr.Error())
	}
	return cause
}

// apiKeyAlreadyIssuedError reports an existing credential for this request
// identity. It names the key when the connector could find it, and never
// deletes it: the connector cannot prove the earlier attempt's secret was never
// delivered, so removing it could revoke a credential a caller already holds.
//
// The guidance differs by whether a key was located, because the two cases have
// different recoveries. Any recorded operation spends the request id, so the
// caller always has to raise a NEW request; when a key exists it must also be
// dealt with first, and when none was found there is nothing to revoke and
// saying otherwise would send an operator looking for a key that is not there.
func apiKeyAlreadyIssuedError(requestID, ownerID, keyID string) error {
	if keyID == "" {
		return status.Errorf(codes.AlreadyExists,
			"baton-temporalcloud: an earlier attempt for request %q (service account %q) is recorded at the provider, "+
				"and no key for it was found. Temporal Cloud returns a key's secret only once, so this request id is spent: "+
				"raise a new request.",
			requestID, ownerID)
	}
	return status.Errorf(codes.AlreadyExists,
		"baton-temporalcloud: an API key for request %q already exists for service account %q (key %s). "+
			"Temporal Cloud returns a key's secret only once, so it cannot be re-delivered and this request id is spent: "+
			"revoke that key if it is not in use, then raise a new request.",
		requestID, ownerID, keyID)
}

// issuedCredentialName is the provider-side display name this connector gives
// a credential it mints. Deriving it from the request id is what makes a
// retried request find its predecessor's key.
func issuedCredentialName(requestID string) string {
	return apiKeyNamePrefix + requestID
}

// apiKeyOperationID is the provider request identity for a C1 issuance
// request: the value sent as CreateApiKeyRequest.async_operation_id and later
// read back with GetAsyncOperation. It is a pure function of the C1 request id,
// so every attempt at the same request carries the same identity, and a
// response-lost create can be detected without depending on the key listing.
func apiKeyOperationID(requestID string) string {
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
		// Truncated to a whole second so the instant this connector asks for is
		// the instant it can compare against what the provider reports back;
		// a sub-second component would make every default-path readback fail
		// on rounding alone.
		return now.Add(apiKeyDefaultTTL).Truncate(time.Second), nil
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
