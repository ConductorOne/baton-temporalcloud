package client

import (
	"context"
	"errors"
	"fmt"

	"google.golang.org/grpc/credentials"
)

type apiKeyCredentials struct {
	apiKey        string
	allowInsecure bool
}

var _ credentials.PerRPCCredentials = (*apiKeyCredentials)(nil)

func NewAPIKeyCredentials(apiKey string, allowInsecure bool) credentials.PerRPCCredentials {
	return &apiKeyCredentials{
		apiKey:        apiKey,
		allowInsecure: allowInsecure,
	}
}

func (c *apiKeyCredentials) GetRequestMetadata(ctx context.Context, uri ...string) (map[string]string, error) {
	ri, ok := credentials.RequestInfoFromContext(ctx)
	if !ok {
		return nil, errors.New("failed to retrieve request info from context")
	}

	if !c.allowInsecure {
		err := credentials.CheckSecurityLevel(ri.AuthInfo, credentials.PrivacyAndIntegrity)
		if err != nil {
			return nil, fmt.Errorf("failed to meet required transport security level: %w", err)
		}
	}
	m := map[string]string{
		"Authorization": fmt.Sprintf("Bearer %s", c.apiKey),
	}
	return m, nil
}

func (c *apiKeyCredentials) RequireTransportSecurity() bool {
	return !c.allowInsecure
}
