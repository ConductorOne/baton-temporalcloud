// Adapted from github.com/conductorone/baton-multipass-sdk/pkg/multipass v0.0.1 (b9f4b677). Replace with that module once it is public (IGA-5320).

// Package multipass serializes the API key value this connector issues under
// Multipass's api_key_v2 content type. The struct matches the field names
// Multipass decodes, mirroring
// crates/latchkey-client-sdk/src/secret_types/definitions.rs at
// ductone/multipass a5d4dad2dfb3a7ec820bf78fc7135ce6c8eda531. It does no
// validation: Multipass owns the schema, and a value it rejects must come back
// as its error rather than as a connector-side guess.
package multipass

import (
	"encoding/json"
	"fmt"

	configv1 "github.com/conductorone/baton-sdk/pb/c1/config/v1"
	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	"github.com/conductorone/baton-sdk/pkg/field"
)

// ContentType is the MIME type that carries an APIKeyV2.
const ContentType = "application/vnd.multipass.api_key_v2+json"

// APIKeyV2 is Multipass's api_key_v2 value. The JSON_V1 codec in Multipass
// writes keys in declaration order, omits empty optionals, and rejects unknown
// keys and nulls, so every optional field carries omitempty.
type APIKeyV2 struct {
	KeyValue   string   `json:"key_value"`
	Provider   string   `json:"provider,omitempty"`
	BaseURL    string   `json:"base_url,omitempty"`
	Scopes     []string `json:"scopes,omitempty"`
	KeyID      string   `json:"key_id,omitempty"`
	HeaderName string   `json:"header_name,omitempty"`
	// ExpiresAt is a date in YYYY-MM-DD form.
	ExpiresAt string `json:"expires_at,omitempty"`
}

// Tags classifies a field carrying an APIKeyV2, for display and debugging in C1.
func Tags() []string {
	return []string{string(field.FieldTagCredential), string(field.FieldTagAPIKey), "multipass", "api_key_v2"}
}

// OutputField declares the credential a descriptor returns: its content type,
// its tags, and that the value is a secret. Field.content_type is only defined
// for the string-shaped fields, so the oneof is set to an empty StringField.
func OutputField(name string) *configv1.Field {
	return configv1.Field_builder{
		Name:        name,
		ContentType: ContentType,
		Tags:        Tags(),
		IsSecret:    true,
		StringField: configv1.StringField_builder{}.Build(),
	}.Build()
}

// Plaintext serializes v and sets the schema, so the ciphertext carries the
// same content type.
func Plaintext(name string, v APIKeyV2) (*v2.PlaintextData, error) {
	encoded, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("secret: encode api_key_v2: %w", err)
	}
	return v2.PlaintextData_builder{
		Name:   name,
		Schema: ContentType,
		Bytes:  encoded,
	}.Build(), nil
}
