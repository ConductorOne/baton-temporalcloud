#!/bin/bash

cd /Users/laurenleach/go/src/github.com/ConductorOne/baton-temporalcloud

git add -A

git commit -m "Containerize baton-temporalcloud

This PR adds containerization support to baton-temporalcloud following the patterns from baton-databricks and baton-contentful.

Changes include:
- Created pkg/config/config.go with field definitions
- Created pkg/config/gen/gen.go to generate config struct
- Added generated pkg/config/conf.gen.go with TemporalCloud config struct
- Updated cmd/baton-temporalcloud/main.go to use new config pattern
- Updated Makefile to include config generation target
- Updated CI workflow to check config is in sync

Co-Authored-By: Claude Sonnet 4.5 <noreply@anthropic.com>"

git push -u origin containerize-temporalcloud

echo "Done! Branch pushed to origin."
