package config

//go:generate go run ./gen

import (
	"github.com/conductorone/baton-sdk/pkg/field"
)

var (
	APIKeyField = field.StringField(
		"api-key",
		field.WithRequired(true),
		field.WithDescription("The Temporal Cloud API key used to connect to the Temporal Cloud API."),
	)
	AllowInsecureField = field.BoolField(
		"allow-insecure",
		field.WithDefaultValue(false),
		field.WithDescription("Allow insecure TLS connections to the Temporal Cloud API."),
	)
	DefaultAccountRoleField = field.StringField(
		"default-account-role",
		field.WithRequired(false),
		field.WithDescription("The default account role to use for account provisioning, must be one of [read, developer, admin]"),
		field.WithDefaultValue("read"),
		field.WithString(func(r *field.StringRuler) {
			r.In([]string{"read", "developer", "admin"})
		}),
	)

	// ConfigurationFields defines the external configuration required for the
	// connector to run.
	ConfigurationFields = []field.SchemaField{
		APIKeyField,
		AllowInsecureField,
		DefaultAccountRoleField,
	}

	// Config is the configuration schema for the connector.
	Config = field.Configuration{
		Fields: ConfigurationFields,
	}
)

// ValidateConfig is run after the configuration is loaded, and should return an
// error if it isn't valid. Implementing this function is optional, it only
// needs to perform extra validations that cannot be encoded with configuration
// parameters.
func ValidateConfig(c *TemporalCloud) error {
	return nil
}
