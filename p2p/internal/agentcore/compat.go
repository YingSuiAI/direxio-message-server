package agentcore

import (
	"context"
	"errors"
)

// Config is the small compatibility surface kept for the retained deployment
// dashboard tests. The retired gRPC agent surface no longer exists here.
type Config struct {
	DeploymentLedger DeploymentLedger
	OwnerID          func() string
}

// Client is the compatibility wrapper used by the retained dashboard tests.
type Client struct{ cfg Config }

func New(cfg Config) (*Client, error) {
	if cfg.DeploymentLedger == nil {
		return nil, errors.New("deployment ledger is required")
	}
	return &Client{cfg: cfg}, nil
}

func (c *Client) owner() string {
	if c != nil && c.cfg.OwnerID != nil {
		return c.cfg.OwnerID()
	}
	return ""
}

func (c *Client) dashboardGet(ctx context.Context, _ map[string]any) (any, *error) {
	summary := map[string]any{
		"deployment_pending":    int64(0),
		"deployment_running":    int64(0),
		"deployment_succeeded":  int64(0),
		"deployment_failed":     int64(0),
		"deployment_destroyed":  int64(0),
		"task_pending":          int64(0),
		"task_running":          int64(0),
		"task_completed":        int64(0),
		"task_failed":           int64(0),
		"schedule_active":       int64(0),
		"schedule_paused":       int64(0),
		"confirmation_pending":  int64(0),
		"server_count":          int64(0),
		"estimated_monthly_usd": float64(0),
		"estimated_accrued_usd": float64(0),
	}
	recent, _, err := c.cfg.DeploymentLedger.ListDeployments(ctx, c.owner(), DeploymentListOptions{PageSize: 20})
	if err != nil {
		return nil, &err
	}
	return map[string]any{"summary": summary, "recent_deployments": recent, "warnings": []string{}, "partial": true}, nil
}
