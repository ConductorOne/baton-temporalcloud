package main

import (
	cfg "github.com/conductorone/baton-temporalcloud/pkg/config"
	"github.com/conductorone/baton-sdk/pkg/config"
)

func main() {
	config.Generate("temporal-cloud", cfg.Config)
}
