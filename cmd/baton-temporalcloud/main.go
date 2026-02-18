package main

import (
	"context"

	"github.com/conductorone/baton-sdk/pkg/config"
	"github.com/conductorone/baton-sdk/pkg/connectorrunner"
	cfg "github.com/conductorone/baton-temporalcloud/pkg/config"
	"github.com/conductorone/baton-temporalcloud/pkg/connector"
)

var version = "dev"

func main() {
	ctx := context.Background()
	config.RunConnector(ctx,
		"baton-temporalcloud",
		version,
		cfg.Config,
		connector.New,
		connectorrunner.WithDefaultCapabilitiesConnectorBuilderV2(&connector.Connector{}),
	)
}
