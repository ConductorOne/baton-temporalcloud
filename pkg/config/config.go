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
	DefaultAccountRoleField = field.SelectField(
		"default-account-role",
		[]string{"read", "developer", "admin"},
		field.WithDisplayName("Default Account Role"),
		field.WithDescription("The default account role to use for account provisioning"),
		field.WithDefaultValue("read"),
		field.WithRequired(false),
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
