// Adapted from github.com/conductorone/baton-multipass-sdk/pkg/multipass v0.0.1 (b9f4b677). Replace with that module once it is public (IGA-5320).

package multipass

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestAPIKeyV2JSON pins the exact bytes the value serializes to: key names and
// their absence for unset fields are part of the Multipass format.
func TestAPIKeyV2JSON(t *testing.T) {
	full, err := json.Marshal(APIKeyV2{
		KeyValue:   "dd-key",
		Provider:   "datadog",
		BaseURL:    "https://api.datadoghq.com",
		Scopes:     []string{"read"},
		KeyID:      "key-id",
		HeaderName: "DD-API-KEY",
		ExpiresAt:  "2027-01-01",
	})
	require.NoError(t, err)
	require.Equal(t, `{"key_value":"dd-key","provider":"datadog","base_url":"https://api.datadoghq.com","scopes":["read"],"key_id":"key-id","header_name":"DD-API-KEY","expires_at":"2027-01-01"}`, string(full))

	minimal, err := json.Marshal(APIKeyV2{KeyValue: "dd-key"})
	require.NoError(t, err)
	require.Equal(t, `{"key_value":"dd-key"}`, string(minimal))
}

func TestOutputFieldDeclaresTheType(t *testing.T) {
	field := OutputField("api_key")
	require.Equal(t, "api_key", field.GetName())
	require.Equal(t, "application/vnd.multipass.api_key_v2+json", field.GetContentType())
	require.Equal(t, []string{"credential", "api_key", "multipass", "api_key_v2"}, field.GetTags())
	require.True(t, field.GetIsSecret())
	require.True(t, field.HasStringField(), "content_type is only valid on the string-shaped fields")
	require.NoError(t, field.Validate())
}

func TestPlaintextSetsSchema(t *testing.T) {
	plaintext, err := Plaintext("api_key", APIKeyV2{KeyValue: "dd-key"})
	require.NoError(t, err)
	require.Equal(t, "api_key", plaintext.GetName())
	require.Equal(t, "application/vnd.multipass.api_key_v2+json", plaintext.GetSchema())
	require.JSONEq(t, `{"key_value":"dd-key"}`, string(plaintext.GetBytes()))
}
