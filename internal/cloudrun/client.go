package cloudrun

import (
	"context"
	"errors"
	"fmt"

	"google.golang.org/api/option"
	run "google.golang.org/api/run/v1"
)

// Client is a Cloud Run Admin API client bound to a project/region.
// Everything that calls the API is added as a method on this type, so callers (cmd/) do not
// have to carry project/region around on every call.
type Client struct {
	api     *run.APIService
	project string
	region  string
}

// NewClient creates a Cloud Run Admin API client. Authentication uses the local
// Application Default Credentials, which run.NewService discovers automatically.
// The v1 namespaces API requires a regional endpoint, so region is mandatory.
//
// opts are appended after the default endpoint option, so tests can swap in a fake API with
// option.WithEndpoint / option.WithHTTPClient.
func NewClient(ctx context.Context, project, region string, opts ...option.ClientOption) (*Client, error) {
	if project == "" {
		return nil, errors.New("project is required")
	}
	if region == "" {
		return nil, errors.New("region is required")
	}

	all := append([]option.ClientOption{option.WithEndpoint(regionalEndpoint(region))}, opts...)
	api, err := run.NewService(ctx, all...)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize the Cloud Run client: %w", err)
	}
	return &Client{api: api, project: project, region: region}, nil
}

// Project returns the client's target project.
func (c *Client) Project() string { return c.project }

// Region returns the client's target region.
func (c *Client) Region() string { return c.region }

// regionalEndpoint builds the regional endpoint for the v1 namespaces API.
func regionalEndpoint(region string) string {
	return fmt.Sprintf("https://%s-run.googleapis.com", region)
}

// serviceName builds the service resource name for the namespaces API.
func (c *Client) serviceName(service string) string {
	return fmt.Sprintf("namespaces/%s/services/%s", c.project, service)
}

// parent builds the parent resource name for the namespaces API (namespaces/<project>).
func (c *Client) parent() string {
	return fmt.Sprintf("namespaces/%s", c.project)
}

// GetService fetches the definition of the given service from the Cloud Run Admin API.
func (c *Client) GetService(ctx context.Context, service string) (*run.Service, error) {
	obj, err := c.api.Namespaces.Services.Get(c.serviceName(service)).Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("failed to get service %q: %w", service, err)
	}
	return obj, nil
}
