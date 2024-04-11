![Baton Logo](./docs/images/baton-logo.png)

# `baton-temporalcloud` [![Go Reference](https://pkg.go.dev/badge/github.com/conductorone/baton-temporalcloud.svg)](https://pkg.go.dev/github.com/conductorone/baton-temporalcloud) ![main ci](https://github.com/conductorone/baton-temporalcloud/actions/workflows/main.yaml/badge.svg)

`baton-temporalcloud` is a connector for [Temporal Cloud](https://temporal.io/cloud) built using the [Baton SDK](https://github.com/conductorone/baton-sdk). It communicates with the Temporal Cloud API to sync data about namespace permissions, account roles, and users.

Check out [Baton](https://github.com/conductorone/baton) to learn more the project in general.

# Getting Started

## brew

```
brew install conductorone/baton/baton conductorone/baton/baton-temporalcloud
BATON_API_KEY=temporal_cloud_api_key baton-temporalcloud
baton resources
```

## docker

```
docker run --rm -v $(pwd):/out -e BATON_API_KEY=temporal_cloud_api_key ghcr.io/conductorone/baton-temporalcloud:latest -f "/out/sync.c1z"
docker run --rm -v $(pwd):/out ghcr.io/conductorone/baton:latest -f "/out/sync.c1z" resources
```

## source

```
go install github.com/conductorone/baton/cmd/baton@main
go install github.com/conductorone/baton-temporalcloud/cmd/baton-temporalcloud@main

BATON_API_KEY=temporal_cloud_api_key baton-temporalcloud
baton resources
```

# Data Model

`baton-temporalcloud` will pull down information about the following Temporal Cloud resources:
- Namespaces
- Users
- Account Roles

# Contributing, Support and Issues

We started Baton because we were tired of taking screenshots and manually building spreadsheets. We welcome contributions, and ideas, no matter how small -- our goal is to make identity and permissions sprawl less painful for everyone. If you have questions, problems, or ideas: Please open a Github Issue!

See [CONTRIBUTING.md](https://github.com/ConductorOne/baton/blob/main/CONTRIBUTING.md) for more details.

# `baton-temporalcloud` Command Line Usage

```
baton-temporalcloud

Usage:
  baton-temporalcloud [flags]
  baton-temporalcloud [command]

Available Commands:
  capabilities       Get connector capabilities
  completion         Generate the autocompletion script for the specified shell
  help               Help about any command

Flags:
      --allow-insecure         Allow insecure TLS connections to the Temporal Cloud API. ($BATON_ALLOW_INSECURE)
      --api-key string         The Temporal Cloud API key used to connect to the Temporal Cloud API. ($BATON_API_KEY)
      --client-id string       The client ID used to authenticate with ConductorOne ($BATON_CLIENT_ID)
      --client-secret string   The client secret used to authenticate with ConductorOne ($BATON_CLIENT_SECRET)
  -f, --file string            The path to the c1z file to sync with ($BATON_FILE) (default "sync.c1z")
  -h, --help                   help for baton-temporalcloud
      --log-format string      The output format for logs: json, console ($BATON_LOG_FORMAT) (default "json")
      --log-level string       The log level: debug, info, warn, error ($BATON_LOG_LEVEL) (default "info")
  -p, --provisioning           This must be set in order for provisioning actions to be enabled. ($BATON_PROVISIONING)
  -v, --version                version for baton-temporalcloud

Use "baton-temporalcloud [command] --help" for more information about a command.

```