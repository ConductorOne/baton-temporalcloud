package config

//go:generate go run ./gen

import (
	"github.com/conductorone/baton-sdk/pkg/field"
)

var (
	APIKeyField = field.StringField(
		"api-key",
		field.WithDisplayName("API Key"),
		field.WithDescription("The Temporal Cloud API key used to connect to the Temporal Cloud API."),
		field.WithIsSecret(true),
		field.WithRequired(true),
	)
	AllowInsecureField = field.BoolField(
		"allow-insecure",
		field.WithDisplayName("Allow Insecure"),
		field.WithDescription("Allow insecure TLS connections to the Temporal Cloud API."),
		field.WithDefaultValue(false),
	)
	DefaultAccountRoleField = field.StringField(
		"default-account-role",
		field.WithDisplayName("Default Account Role"),
		field.WithDescription("The default account role to use for account provisioning, must be one of [read, developer, admin]"),
		field.WithDefaultValue("read"),
		field.WithRequired(false),
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

	// FieldRelationships defines relationships between the fields listed in
	// ConfigurationFields that can be automatically validated.
	FieldRelationships = []field.SchemaFieldRelationship{}

	// Config is the configuration schema for the connector.
	Config = field.NewConfiguration(
		ConfigurationFields,
		field.WithConstraints(FieldRelationships...),
		field.WithConnectorDisplayName("Temporal Cloud"),
		field.WithHelpUrl("/docs/baton/temporalcloud"),
		field.WithIconUrl("/static/app-icons/temporalcloud.svg"),
	)
)

// ValidateConfig is run after the configuration is loaded, and should return an
// error if it isn't valid. Implementing this function is optional, it only
// needs to perform extra validations that cannot be encoded with configuration
// parameters.
func ValidateConfig(c *TemporalCloud) error {
	return nil
}
