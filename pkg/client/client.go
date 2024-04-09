package client

import (
	"context"
	"crypto/tls"
	_ "embed"
	"fmt"
	"net/url"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
)

//go:embed version
var temporalCloudAPIVersion string

func NewConnectionWithAPIKey(addrStr string, allowInsecure bool, apiKey string, opts ...grpc.DialOption) (*grpc.ClientConn, error) {
	creds := NewAPIKeyCredentials(apiKey, allowInsecure)
	opts = append(opts, grpc.WithPerRPCCredentials(creds))
	return newConnection(addrStr, allowInsecure, opts...)
}

func newConnection(addrStr string, allowInsecure bool, opts ...grpc.DialOption) (*grpc.ClientConn, error) {
	addr, err := url.Parse(addrStr)
	if err != nil {
		return nil, fmt.Errorf("unable to parse server address: %w", err)
	}
	defaultOpts := defaultDialOptions(addr, allowInsecure)
	opts = append(opts, defaultOpts...)
	conn, err := grpc.NewClient(addr.String(), opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to dial `%s`: %w", addr.String(), err)
	}

	return conn, nil
}

func defaultDialOptions(addr *url.URL, allowInsecure bool) []grpc.DialOption {
	var opts []grpc.DialOption

	transport := credentials.NewTLS(
		&tls.Config{
			MinVersion: tls.VersionTLS12,
			ServerName: addr.Hostname(),
		})
	if allowInsecure {
		transport = insecure.NewCredentials()
	}

	opts = append(opts, grpc.WithTransportCredentials(transport), grpc.WithUnaryInterceptor(apiVersionInterceptor))

	return opts
}

func apiVersionInterceptor(ctx context.Context, method string, req any, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
	ctx = metadata.AppendToOutgoingContext(ctx, "temporal-cloud-api-version", strings.TrimSpace(temporalCloudAPIVersion))
	return invoker(ctx, method, req, reply, cc, opts...)
}
