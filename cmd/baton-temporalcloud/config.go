package main

import (
	"context"
	"errors"

	"github.com/conductorone/baton-sdk/pkg/cli"
	"github.com/spf13/cobra"
)

// config defines the external configuration required for the connector to run.
type config struct {
	cli.BaseConfig `mapstructure:",squash"` // Puts the base config options in the same place as the connector options
	APIKey         string                   `mapstructure:"api-key"`
	AllowInsecure  bool                     `mapstructure:"allow-insecure"`
}

// validateConfig is run after the configuration is loaded, and should return an error if it isn't valid.
func validateConfig(ctx context.Context, cfg *config) error {
	if cfg.APIKey == "" {
		return errors.New("API key is missing")
	}

	return nil
}

func cmdFlags(cmd *cobra.Command) {
	cmd.PersistentFlags().String("api-key", "", "The Temporal Cloud API key used to connect to the Temporal Cloud API. ($BATON_API_KEY)")
	cmd.PersistentFlags().Bool("allow-insecure", false, "Allow insecure TLS connections to the Temporal Cloud API. ($BATON_ALLOW_INSECURE)")
}
