package cloudrun

import (
	"context"
	"errors"
	"fmt"

	"google.golang.org/api/option"
	run "google.golang.org/api/run/v1"
)

// Client は project/region に紐づいた Cloud Run Admin API クライアント。
// API を叩く処理はすべてこの型のメソッドとして生やし、呼び出し側 (cmd/) が
// project/region を毎回引き回さなくて済むようにする。
type Client struct {
	api     *run.APIService
	project string
	region  string
}

// NewClient は Cloud Run Admin API クライアントを生成する。認証はローカルの
// Application Default Credentials で、run.NewService が自動的に検出する。
// v1 namespaces API はリージョナルエンドポイントを必要とするため region は必須。
//
// opts は既定のエンドポイント設定の後ろに追加されるので、テストから
// option.WithEndpoint / option.WithHTTPClient でフェイク API に差し替えられる。
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

// Project はクライアントの対象プロジェクトを返す。
func (c *Client) Project() string { return c.project }

// Region はクライアントの対象リージョンを返す。
func (c *Client) Region() string { return c.region }

// regionalEndpoint は v1 namespaces API のリージョナルエンドポイントを組み立てる。
func regionalEndpoint(region string) string {
	return fmt.Sprintf("https://%s-run.googleapis.com", region)
}

// serviceName は namespaces API のサービスリソース名を組み立てる。
func (c *Client) serviceName(service string) string {
	return fmt.Sprintf("namespaces/%s/services/%s", c.project, service)
}

// parent は namespaces API の親リソース名 (namespaces/<project>) を組み立てる。
func (c *Client) parent() string {
	return fmt.Sprintf("namespaces/%s", c.project)
}

// GetService は指定したサービスの定義を Cloud Run Admin API から取得する。
func (c *Client) GetService(ctx context.Context, service string) (*run.Service, error) {
	obj, err := c.api.Namespaces.Services.Get(c.serviceName(service)).Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("failed to get service %q: %w", service, err)
	}
	return obj, nil
}
