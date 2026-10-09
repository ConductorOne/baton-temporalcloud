package connector

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestAPIKeyV2ValueIsTheCanonicalDocument pins the value bytes against the
// rules the shipped Multipass codec enforces, because the vault authenticates
// these bytes and a client decodes them: a document that merely looks like JSON
// is not the contract.
//
// The rules under test, from the codec at ductone/multipass @ 8826d8ac:
//
//   - a flat JSON object keyed by the schema's field names;
//   - keys in declaration order (key_value, then key_id);
//   - empty optional fields omitted, so a key with no provider id carries one
//     field and not an empty string;
//   - no HTML escaping, which is where encoding/json's default and serde_json
//     disagree;
//   - no trailing newline, which is where encoding/json's Encoder and
//     serde_json's writer disagree.
//
// Each case is a literal rather than a round-trip through the encoder, so the
// test cannot agree with the implementation by construction.
func TestAPIKeyV2ValueIsTheCanonicalDocument(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		keyValue string
		keyID    string
		want     string
	}{
		{
			name:     "both fields, declaration order",
			keyValue: "abc-123",
			keyID:    "key-1",
			want:     `{"key_value":"abc-123","key_id":"key-1"}`,
		},
		{
			name:     "key_id omitted when the provider reported none",
			keyValue: "abc-123",
			want:     `{"key_value":"abc-123"}`,
		},
		{
			name:     "characters encoding/json escapes by default are left alone",
			keyValue: "a<b>c&d",
			keyID:    "k",
			want:     `{"key_value":"a<b>c&d","key_id":"k"}`,
		},
		{
			name:     "the JSON minimum is still escaped",
			keyValue: "a\"b\\c\nd",
			want:     `{"key_value":"a\"b\\c\nd"}`,
		},
		{
			name:     "an opaque JSON token stays a string, never a nested object",
			keyValue: `{"key":"abc"}`,
			want:     `{"key_value":"{\"key\":\"abc\"}"}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := apiKeyV2Value(tc.keyValue, tc.keyID)
			require.NoError(t, err)
			require.Equal(t, tc.want, string(got))
			require.False(t, strings.HasSuffix(string(got), "\n"),
				"the codec's writer emits no trailing newline")
		})
	}
}
