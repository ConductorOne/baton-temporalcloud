package connector

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/grpc-ecosystem/go-grpc-middleware/logging/zap/ctxzap"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/conductorone/baton-multipass-sdk/pkg/multipass"
	configv1 "github.com/conductorone/baton-sdk/pb/c1/config/v1"
	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	"github.com/conductorone/baton-sdk/pkg/actions"
	"github.com/conductorone/baton-sdk/pkg/annotations"
	rs "github.com/conductorone/baton-sdk/pkg/types/resource"

	cloudservicev1 "go.temporal.io/cloud-sdk/api/cloudservice/v1"
	identityv1 "go.temporal.io/cloud-sdk/api/identity/v1"
)

// The service-account API key type carries one issuer: a resource-scoped
// ACTION_TYPE_RESOURCE_CREATE action that mints a key for a service account.
// C1 recognizes it as a credential issuer by its schema alone: the action type,
// the one secret return whose content type is a Multipass type, and the
// resource return that carries the created key. Revoking an issued key is the
// resource delete the key type's syncer already provides.
//
// User-owned keys have no issuer. Only service accounts are vended for; a user
// key type exists so the two owner kinds are not mixed under one resource type.

const (
	// ActionIssueServiceAccountAPIKey is the issuer's action name.
	ActionIssueServiceAccountAPIKey = "issue_service_account_api_key"

	// actionArgServiceAccountID is the subject: the service account that owns
	// the key. C1 supplies it from the request.
	actionArgServiceAccountID = "service_account_id"
	// actionArgLifetimeDays is an admin-fixed offering value: how long the
	// provider keeps the key alive. Temporal Cloud requires every API key to
	// carry an expiry, and the handler is given no requested expiry, so the
	// lifetime has to be part of the offering.
	actionArgLifetimeDays = "lifetime_days"

	// actionReturnResource is the created key's resource, carrying its
	// SecretTrait.
	actionReturnResource = "resource"
	// apiKeySecretFieldName names the secret return and its plaintext.
	apiKeySecretFieldName = "temporal_cloud_api_key"

	// apiKeyProvider is the Multipass api_key_v2 provider value.
	apiKeyProvider = "temporalcloud"

	// apiKeyMinLifetimeDays and apiKeyMaxLifetimeDays bound lifetime_days.
	// The ceiling is Temporal Cloud's documented maximum API-key expiry of two
	// years (https://docs.temporal.io/cloud/api-keys); the floor is the
	// connector's own, since the provider documents none and a key shorter than
	// a day is not useful to vend.
	apiKeyMinLifetimeDays = 1
	apiKeyMaxLifetimeDays = 2 * 365

	// issuedKeyNameSuffixLen is the length of the random suffix of an issued
	// key's name: 6 base32 characters, 30 bits.
	issuedKeyNameSuffixLen = 6
)

// serviceAccountAPIKeyActionSchema is the issuer's schema.
func serviceAccountAPIKeyActionSchema() *v2.BatonActionSchema {
	secret := multipass.OutputField(apiKeySecretFieldName)
	secret.SetIsRequired(true)
	return v2.BatonActionSchema_builder{
		Name:        ActionIssueServiceAccountAPIKey,
		DisplayName: "Issue Temporal Cloud service account API key",
		Description: "Creates an API key for a Temporal Cloud service account and returns it as a Multipass api_key_v2 secret.",
		Arguments: []*configv1.Field{
			configv1.Field_builder{
				Name:        actionArgServiceAccountID,
				DisplayName: "Service account",
				Description: "The Temporal Cloud service account that owns the key.",
				IsRequired:  true,
				ResourceIdField: configv1.ResourceIdField_builder{
					Rules: configv1.ResourceIDRules_builder{
						AllowedResourceTypeIds: []string{serviceAccountResourceType.Id},
					}.Build(),
				}.Build(),
			}.Build(),
			configv1.Field_builder{
				Name:        actionArgLifetimeDays,
				DisplayName: "Key lifetime (days)",
				Description: fmt.Sprintf(
					"Days until Temporal Cloud expires the key, %d to %d. Set it at least as long as the offering duration: "+
						"C1 revokes at the earlier of the two.",
					apiKeyMinLifetimeDays, apiKeyMaxLifetimeDays),
				IsRequired: true,
				IntField: configv1.IntField_builder{
					Rules: configv1.Int64Rules_builder{
						Gte: proto64(apiKeyMinLifetimeDays),
						Lte: proto64(apiKeyMaxLifetimeDays),
					}.Build(),
				}.Build(),
			}.Build(),
		},
		ReturnTypes: []*configv1.Field{
			secret,
			configv1.Field_builder{
				Name:          actionReturnResource,
				DisplayName:   "Secret resource",
				Description:   "The created key, so it can be revoked.",
				IsRequired:    true,
				ResourceField: configv1.ResourceField_builder{}.Build(),
			}.Build(),
		},
		ActionType: []v2.ActionType{v2.ActionType_ACTION_TYPE_RESOURCE_CREATE},
	}.Build()
}

func proto64(v int64) *int64 { return &v }

// ResourceActions registers the issuer on the service-account key type only.
// The user key type has no issuer, and a type may carry at most one.
func (o *apiKeyBuilder) ResourceActions(ctx context.Context, registry actions.ActionRegistry) error {
	if o.kind.resourceType.Id != serviceAccountAPIKeyResourceType.Id {
		return nil
	}
	if err := actions.RegisterWithSecrets(ctx, registry, serviceAccountAPIKeyActionSchema(), o.issue); err != nil {
		return fmt.Errorf("baton-temporalcloud: failed to register %s action: %w", ActionIssueServiceAccountAPIKey, err)
	}
	return nil
}

// issue mints one Temporal Cloud API key owned by the requested service
// account. It never creates or modifies an identity: the owner must already
// exist, and the key inherits the owner's permissions.
//
// # Idempotency
//
// The handler receives no request id, so it has none to deduplicate on and does
// not try. Every call creates a new key under a fresh name
// (c1-<UTC>-<6 random>) and returns its secret. Temporal Cloud returns a key's
// secret exactly once, at creation, so a retry could never re-deliver an earlier
// key anyway; a repeated call is a new credential, and C1 owns the decision to
// call again. What the handler guarantees is narrower: once CreateApiKey has
// returned a key id, every later failure deletes the key it made. A create whose
// response is lost never yields an id, so the handler cannot undo it; that key
// shows up in the next sync under its owner.
//
// # Awaiting the create
//
// The create is an asynchronous provider operation. The handler awaits it, then
// reads the key back and checks owner, owner type and expiry against what was
// asked for, before anything is returned. A key whose creation cannot be
// confirmed is deleted rather than handed out.
//
// # Undo
//
// Once the create has returned a key id, any failure to return the result (the
// operation failed or timed out, the read-back disagrees, the secret is missing,
// the result cannot be built) deletes the key, on a context detached from the
// caller's so a canceled request cannot strand it. A failed undo is reported
// next to the original cause.
func (o *apiKeyBuilder) issue(
	ctx context.Context,
	args *structpb.Struct,
) (*structpb.Struct, []*v2.PlaintextData, annotations.Annotations, error) {
	owner, err := actions.RequireResourceIDArg(args, actionArgServiceAccountID)
	if err != nil {
		return nil, nil, nil, status.Errorf(codes.InvalidArgument, "baton-temporalcloud: %v", err)
	}
	if owner.GetResourceType() != serviceAccountResourceType.Id || owner.GetResource() == "" {
		return nil, nil, nil, status.Errorf(codes.InvalidArgument,
			"baton-temporalcloud: %s must be a %s resource", actionArgServiceAccountID, serviceAccountResourceType.Id)
	}
	days, ok := actions.GetIntArg(args, actionArgLifetimeDays)
	if !ok || days < apiKeyMinLifetimeDays || days > apiKeyMaxLifetimeDays {
		return nil, nil, nil, status.Errorf(codes.InvalidArgument,
			"baton-temporalcloud: %s is required and must be %d to %d", actionArgLifetimeDays, apiKeyMinLifetimeDays, apiKeyMaxLifetimeDays)
	}

	now := time.Now()
	name, err := newIssuedKeyName(now, rand.Reader)
	if err != nil {
		return nil, nil, nil, err
	}
	// Truncated to a whole second so the instant asked for is the instant that
	// can be compared with what the provider reports back.
	approvedExpiry := now.Add(time.Duration(days) * 24 * time.Hour).Truncate(time.Second)

	resp, err := o.client.CreateApiKey(ctx, &cloudservicev1.CreateApiKeyRequest{
		Spec: &identityv1.ApiKeySpec{
			OwnerId:     owner.GetResource(),
			OwnerType:   identityv1.OwnerType_OWNER_TYPE_SERVICE_ACCOUNT,
			DisplayName: name,
			Description: "ConductorOne vended credential",
			ExpiryTime:  timestamppb.New(approvedExpiry),
		},
	})
	if err != nil {
		return nil, nil, nil, apiKeyCreateError(err)
	}
	keyID := resp.GetKeyId()
	if keyID == "" {
		return nil, nil, nil, status.Error(codes.Internal, "baton-temporalcloud: provider returned an API key with no id")
	}
	if resp.GetToken() == "" {
		return nil, nil, nil, o.undoCreate(ctx, keyID, owner.GetResource(),
			status.Error(codes.Internal, "baton-temporalcloud: provider returned an API key with no secret"))
	}

	if err := o.awaitCreate(ctx, keyID, resp.GetAsyncOperation().GetId(), resp.GetAsyncOperation().GetCheckDuration().AsDuration()); err != nil {
		return nil, nil, nil, o.undoCreate(ctx, keyID, owner.GetResource(), err)
	}
	actualExpiry, err := o.verifyIssuedAPIKey(ctx, keyID, owner.GetResource(), approvedExpiry)
	if err != nil {
		return nil, nil, nil, o.undoCreate(ctx, keyID, owner.GetResource(), err)
	}

	secret, err := rs.NewSecretResource(name, serviceAccountAPIKeyResourceType, keyID, []rs.SecretTraitOption{
		// The SDK requires the secret trait's identity to equal the owning
		// identity, so this is the requested service account.
		rs.WithSecretIdentityID(owner),
		rs.WithSecretType(v2.SecretTrait_CREDENTIAL_TYPE_STATIC_SECRET),
		rs.WithSecretDetail(apiKeyCredentialDetail),
		// The provider's own recorded expiry, not the one asked for.
		rs.WithSecretExpiresAt(actualExpiry),
	}, rs.WithParentResourceID(owner), rs.WithResourceCreatedAt(now))
	if err != nil {
		return nil, nil, nil, o.undoCreate(ctx, keyID, owner.GetResource(),
			fmt.Errorf("baton-temporalcloud: build API key secret resource: %w", err))
	}
	plaintext, err := multipass.Plaintext(apiKeySecretFieldName, multipass.APIKeyV2{
		KeyValue: resp.GetToken(),
		Provider: apiKeyProvider,
		KeyID:    keyID,
	})
	if err != nil {
		return nil, nil, nil, o.undoCreate(ctx, keyID, owner.GetResource(), err)
	}
	field, err := actions.NewResourceReturnField(actionReturnResource, secret)
	if err != nil {
		return nil, nil, nil, o.undoCreate(ctx, keyID, owner.GetResource(),
			fmt.Errorf("baton-temporalcloud: build issuer resource return: %w", err))
	}
	return actions.NewReturnValues(true, field), []*v2.PlaintextData{plaintext}, nil, nil
}

// awaitCreate waits for the provider's asynchronous create of keyID. When the
// create returned no operation there is nothing to await; the read-back's
// NotFound retry covers the provider's own write instead.
func (o *apiKeyBuilder) awaitCreate(ctx context.Context, keyID, opID string, checkEvery time.Duration) error {
	if opID == "" {
		return nil
	}
	l := ctxzap.Extract(ctx).With(zap.String("request_id", opID), zap.String("api_key_id", keyID))
	waitCtx, cancel := context.WithTimeout(ctx, apiKeyCreateMaxDuration)
	defer cancel()
	if err := awaitAsyncOperation(waitCtx, l, o.client, opID, asyncCheckDelay(checkEvery)); err != nil {
		return fmt.Errorf("baton-temporalcloud: API key %q creation did not complete: %w", keyID, err)
	}
	return nil
}

// verifyIssuedAPIKey confirms from the provider's own record that the key it
// created belongs to the requested service account and does not outlive the
// approved expiry. It returns the expiry the provider actually recorded.
//
// The comparison is one-sided. A provider expiry LATER than the approved
// deadline is rejected outright, with no tolerance: the credential would
// outlive the lifetime that was approved. An expiry EARLIER than requested is
// accepted within apiKeyExpiryReadbackTolerance, because the provider rounds
// the instant it stores and a shorter-lived credential is not a safety problem.
func (o *apiKeyBuilder) verifyIssuedAPIKey(ctx context.Context, keyID, ownerID string, approvedExpiry time.Time) (time.Time, error) {
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
	// invalid timestamp is rejected before it is taken as the provider's
	// authoritative instant.
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
// over a short bounded window. Only NotFound is retried: any other error is the
// provider's answer and is returned at once, so a permission or transport
// failure is not mistaken for a slow write.
func (o *apiKeyBuilder) getIssuedAPIKey(ctx context.Context, keyID string) (*cloudservicev1.GetApiKeyResponse, error) {
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

// undoCreate deletes a key this call created but cannot return, and gives back
// cause. A failed undo never replaces the cause: the caller must still learn why
// issuance failed, so the undo failure is appended as text. That also keeps it
// from taking over the error's gRPC code.
//
// The undo runs on a context detached from the caller's and bounded by
// apiKeyDeletionMaxDuration. The caller's context is often already canceled or
// past its deadline by the time we get here (a read-back that failed because the
// request expired is the common way in), and a delete sent on it would never
// leave the process, leaving the key live at the provider.
func (o *apiKeyBuilder) undoCreate(ctx context.Context, keyID, ownerID string, cause error) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), apiKeyDeletionMaxDuration)
	defer cancel()

	if _, err := o.Delete(cleanupCtx, &v2.ResourceId{ResourceType: o.kind.resourceType.Id, Resource: keyID}, nil); err != nil {
		ctxzap.Extract(ctx).Warn(
			"baton-temporalcloud: failed to undo API key creation",
			zap.String("api_key_id", keyID),
			zap.String("service_account_id", ownerID),
			zap.Error(err),
		)
		return fmt.Errorf("%w (undo failed: API key %s for service account %s may remain at the provider: %s)", cause, keyID, ownerID, err.Error())
	}
	return cause
}

// newIssuedKeyName names a key the issuer mints: c1-<UTC time>-<random>, e.g.
// c1-20261110T201426Z-k3x9qa. The clock and randomness are arguments so the name
// is testable.
func newIssuedKeyName(now time.Time, random io.Reader) (string, error) {
	buf := make([]byte, 4)
	if _, err := io.ReadFull(random, buf); err != nil {
		return "", fmt.Errorf("baton-temporalcloud: generate key name: %w", err)
	}
	suffix := strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(buf))[:issuedKeyNameSuffixLen]
	return "c1-" + now.UTC().Format("20060102T150405Z") + "-" + suffix, nil
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
