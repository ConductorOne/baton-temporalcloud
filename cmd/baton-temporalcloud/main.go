package main

import (
	"context"
	"fmt"
	"os"

	configSchema "github.com/conductorone/baton-sdk/pkg/config"
	"github.com/conductorone/baton-sdk/pkg/connectorbuilder"
	"github.com/conductorone/baton-sdk/pkg/field"
	"github.com/conductorone/baton-sdk/pkg/types"
	"github.com/grpc-ecosystem/go-grpc-middleware/logging/zap/ctxzap"
	"github.com/spf13/viper"
	"go.uber.org/zap"

	"github.com/conductorone/baton-temporalcloud/pkg/connector"
)

var defaultAccountRoleOpts = []string{"read", "developer", "admin"}

const (
	version            = "dev"
	connectorName      = "baton-temporalcloud"
	apiKey             = "api-key"
	allowInsecure      = "allow-insecure"
	defaultAccountRole = "default-account-role"
)

var (
	APIKeyField        = field.StringField(apiKey, field.WithRequired(true), field.WithDescription("The Temporal Cloud API key used to connect to the Temporal Cloud API."))
	AllowInsecureField = field.BoolField(
		allowInsecure,
		field.WithDefaultValue(false),
		field.WithDescription("Allow insecure TLS connections to the Temporal Cloud API."),
	)
	DefaultAccountRoleField = field.StringField(
		defaultAccountRole,
		field.WithRequired(false),
		field.WithDescription(fmt.Sprintf("The default account role to use for account provisioning, must be one of %v", defaultAccountRoleOpts)),
		field.WithDefaultValue("read"),
		field.WithString(func(r *field.StringRuler) {
			r.In(defaultAccountRoleOpts)
		}),
	)
	configurationFields = []field.SchemaField{APIKeyField, AllowInsecureField, DefaultAccountRoleField}
)

func main() {
	ctx := context.Background()
	_, cmd, err := configSchema.DefineConfiguration(ctx,
		connectorName,
		getConnector,
		field.NewConfiguration(configurationFields),
	)
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}

	cmd.Version = version
	err = cmd.Execute()
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}
}

func getConnector(ctx context.Context, cfg *viper.Viper) (types.ConnectorServer, error) {
	l := ctxzap.Extract(ctx)
	var opts []connector.Opt
	if cfg.IsSet(defaultAccountRole) && cfg.GetString(defaultAccountRole) != "" {
		opts = append(opts, connector.WithDefaultAccountRole(cfg.GetString(defaultAccountRole)))
	}

	cb, err := connector.New(ctx,
		cfg.GetString(apiKey),
		cfg.GetBool(allowInsecure),
		opts...,
	)
	if err != nil {
		l.Error("error creating connector", zap.Error(err))
		return nil, err
	}

	c, err := connectorbuilder.NewConnector(ctx, cb)
	if err != nil {
		l.Error("error creating connector", zap.Error(err))
		return nil, err
	}

	return c, nil
}
