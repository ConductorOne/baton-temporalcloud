package client

import (
	"context"
	"crypto/tls"
	_ "embed"
	"fmt"
	"net/url"
	"strings"

	cloudservicev1 "go.temporal.io/cloud-sdk/api/cloudservice/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
)

const tmprlCloudAPIAddr = "saas-api.tmprl.cloud:443"

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

var _ cloudservicev1.CloudServiceClient = (*Client)(nil)

type Client struct {
	cloudservicev1.CloudServiceClient

	accountID string
}

func (c *Client) GetAccountID(ctx context.Context) (string, error) {
	if c.accountID != "" {
		return c.accountID, nil
	}

	resp, err := c.GetAccount(ctx, &cloudservicev1.GetAccountRequest{})
	if err != nil {
		return "", err
	}

	return resp.GetAccount().GetId(), nil
}

type config struct {
	allowInsecure bool
	addr          string
}

func newConfig() *config {
	return &config{
		addr: tmprlCloudAPIAddr,
	}
}

type Opt func(c *config) error

func WithAPIAddress(addr string) Opt {
	return func(c *config) error {
		c.addr = addr
		return nil
	}
}

func AllowInsecure() Opt {
	return func(c *config) error {
		c.allowInsecure = true
		return nil
	}
}

func New(apiKey string, opts ...Opt) (*Client, error) {
	cfg := newConfig()
	for _, opt := range opts {
		if err := opt(cfg); err != nil {
			return nil, fmt.Errorf("failed to configure client: %w", err)
		}
	}

	conn, err := NewConnectionWithAPIKey(cfg.addr, cfg.allowInsecure, apiKey)
	if err != nil {
		return nil, fmt.Errorf("failed to create connection: %w", err)
	}
	cClient := cloudservicev1.NewCloudServiceClient(conn)

	return &Client{
		CloudServiceClient: cClient,
	}, nil
}
