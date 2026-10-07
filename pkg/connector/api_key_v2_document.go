package connector

import (
	"bytes"
	"encoding/json"
)

// apiKeyV2ContentType is the content type a native API-key credential carries
// into the vault. It is the identifier the shipped Multipass catalog declares
// for `api_key_v2` (crates/latchkey-client-sdk/src/secret_types/definitions.rs,
// pinned at 8826d8ac); the identifier is frozen once a secret of that type
// exists, because the metadata copy is the value's AEAD associated data.
//
// C1 does not read this constant. It names the document the producer emits so
// the connector and the vault agree on one profile without either inventing a
// second name for it.
const apiKeyV2ContentType = "api_key_v2" //nolint:gosec // Not a credential: a content-type identifier, the same class of false positive C1's own builtin-connector constants carry.

// apiKeyV2Document is the canonical `api_key_v2` value: a flat JSON object
// keyed by the pinned schema's field names, written in declaration order, with
// empty optional fields omitted.
//
// The schema declares, in order:
//
//	key_value     (required)
//	provider      (optional)
//	base_url      (optional)
//	scopes        (optional)
//	key_id        (optional)
//	header_name   (optional)
//	expires_at    (optional)
//
// Only two are populated, and each for a reason:
//
//   - key_value is the credential itself. The pinned schema requires it, and a
//     document missing a required field is refused by the codec rather than
//     rendered blank.
//   - key_id is the provider's own id for the key, reported by the create
//     response. It is the same handle C1 records for revocation, so a reader of
//     the document can correlate the credential with the provider object
//     without a second lookup.
//
// The rest are deliberately absent:
//
//   - provider is the constant "Temporal Cloud"; the document is already
//     addressed by the vault item that holds it.
//   - base_url and header_name are properties of the provider's API, not of
//     this credential, and the connector is not configured with the account's
//     regional endpoint. Guessing either would put a value in the document that
//     nothing verified.
//   - scopes does not apply: a Temporal Cloud API key carries no scopes of its
//     own, it inherits its owner's permissions.
//   - expires_at is a DATE in the pinned schema, while the provider reports an
//     instant. Truncating a reported instant to a date is lossy, and the
//     authoritative expiry already rides the secret trait's `expires_at`
//     untouched.
//
// Field order is load-bearing: the codec writes keys in declaration order, so
// struct order here must match the schema, not convenience.
type apiKeyV2Document struct {
	KeyValue string `json:"key_value"`
	KeyID    string `json:"key_id,omitempty"`
}

// apiKeyV2Value encodes the canonical `api_key_v2` value bytes for one issued
// API key.
//
// The encoding is byte-for-byte what the shipped Rust codec produces for the
// same fields, because the vault authenticates these bytes and a client decodes
// them: compact JSON, declaration order, empty optionals omitted, and no HTML
// escaping. `encoding/json` escapes `<`, `>` and `&` by default, which
// serde_json does not, so the encoder is used with escaping disabled rather
// than `json.Marshal` — the two differ on exactly the bytes that matter here.
func apiKeyV2Value(keyValue, keyID string) ([]byte, error) {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(apiKeyV2Document{KeyValue: keyValue, KeyID: keyID}); err != nil {
		return nil, err
	}
	// Encoder.Encode terminates the document with a newline; the codec's writer
	// does not, and the newline would become part of the authenticated value.
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}
