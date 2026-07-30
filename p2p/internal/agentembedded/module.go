// Package agentembedded exposes the in-process Agent control action facade.
//
// The package deliberately contains no gRPC or generated protobuf dependency.
// Every dependency is a narrow owner-scoped port so the action registry can
// be wired to durable services without creating a second transport contract.
package agentembedded

import (
	"context"
	"errors"
	"net/http"
	"strings"

	actionbase "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/action"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/confirmation"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/task"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/storage"
)

var ErrUnavailable = errors.New("agent embedded capability unavailable")

// ActionPort is used only for domains whose concrete service is owned by a
// different package. Implementations must enforce owner scoping themselves;
// the facade supplies the authenticated owner explicitly.
type ActionPort interface {
	Handle(context.Context, string, string, map[string]any) (any, *actionbase.Error)
}

// ActionPortFunc adapts a closure while keeping owner/action explicit.
type ActionPortFunc func(context.Context, string, string, map[string]any) (any, *actionbase.Error)

func (f ActionPortFunc) Handle(ctx context.Context, owner, action string, params map[string]any) (any, *actionbase.Error) {
	if f == nil {
		return nil, actionbase.CodedError(http.StatusPreconditionFailed, "agent_embedded_unavailable", ErrUnavailable.Error())
	}
	return f(ctx, owner, action, params)
}

// TaskRetryPort and ConfirmationListPort are optional until the underlying
// durable stores expose those operations. They keep the facade precise while
// allowing a store upgrade without transport-specific code here.
type TaskRetryPort interface {
	RetryTask(context.Context, string, string, string, uint64) (task.Task, error)
}

// ScheduleTriggerPort materializes the public schedule run and the generic
// task in one transaction. The returned identifiers are the shared projection
// used by both agent.core.schedules.trigger and agent.schedules.run_now.
type ScheduleTriggerPort interface {
	TriggerSchedule(context.Context, string, string, string) (storage.Schedule, string, string, error)
}

// DeploymentLedger is the dashboard projection port. It intentionally mirrors
// the deployment-ledger contract without importing the retired gRPC adapter.
type DeploymentLedger interface {
	ListDeployments(context.Context, string, DeploymentListOptions) ([]map[string]any, string, error)
	GetDeploymentByID(context.Context, string, string) (map[string]any, bool, error)
	GetDeploymentByWorkloadID(context.Context, string, string) (map[string]any, bool, error)
	ListDeploymentEventsByID(context.Context, string, string, int64, int) ([]map[string]any, int64, error)
	ListDeploymentEventsByWorkloadID(context.Context, string, string, int64, int) ([]map[string]any, int64, error)
}

type DeploymentListOptions struct {
	PageSize   int
	PageToken  string
	Status     string
	TargetKind string
}

type Config struct {
	OwnerID         func() string
	ModelProfiles   storage.ModelProfileStore
	Tasks           task.Store
	TaskRetry       TaskRetryPort
	Confirmations   confirmation.Repository
	Schedules       storage.ScheduleStore
	ScheduleTrigger ScheduleTriggerPort
	MCP             ActionPort
	Skills          ActionPort
	AWS             ActionPort
	GeoLibre        ActionPort
	Workloads       ActionPort
	Deployments     DeploymentLedger
	// CapabilityReady is an optional dynamic fail-closed readiness gate. A
	// capability is published only when its concrete dependency is present and
	// this callback returns true. Tests and small in-memory callers may omit it.
	CapabilityReady func(string) bool
}

type Module struct{ cfg Config }

func New(cfg Config) *Module { return &Module{cfg: cfg} }

// NewModule is kept as a discoverable constructor for callers that avoid the
// short package constructor convention.
func NewModule(cfg Config) *Module { return New(cfg) }

func (m *Module) owner() string {
	if m != nil && m.cfg.OwnerID != nil {
		return strings.TrimSpace(m.cfg.OwnerID())
	}
	return ""
}

func (m *Module) capabilityReady(token string, dependency bool) bool {
	if m == nil || !dependency {
		return false
	}
	if m.cfg.CapabilityReady == nil {
		return true
	}
	return m.cfg.CapabilityReady(token)
}

func unavailable(_ context.Context, _ map[string]any) (any, *actionbase.Error) {
	return nil, actionbase.CodedError(http.StatusPreconditionFailed, "agent_embedded_unavailable", ErrUnavailable.Error())
}

func (m *Module) requireCapability(ctx context.Context, params map[string]any, token string, dependency bool) (any, *actionbase.Error) {
	if !m.capabilityReady(token, dependency) {
		return unavailable(ctx, params)
	}
	return nil, nil
}

func (m *Module) delegated(port ActionPort, action, capability string) actionbase.Handler {
	return func(ctx context.Context, params map[string]any) (any, *actionbase.Error) {
		if _, e := m.requireCapability(ctx, params, capability, port != nil); e != nil {
			return nil, e
		}
		if port == nil {
			return unavailable(ctx, params)
		}
		result, actionErr := port.Handle(ctx, m.owner(), action, params)
		if actionErr != nil {
			return nil, actionErr
		}
		return result, nil
	}
}

// Handlers returns the complete embedded Agent action surface. Unsupported
// local execution paths remain registered and fail before side effects.
func (m *Module) Handlers() map[string]actionbase.Handler {
	h := map[string]actionbase.Handler{
		"agent.backends.get":    m.backendsGet,
		"agent.core.status.get": m.statusGet,
	}
	for _, name := range []string{"agent.core.model_profiles.sync", "agent.core.model_profiles.list", "agent.core.model_profiles.get", "agent.core.model_profiles.delete"} {
		h[name] = m.modelHandler(name)
	}
	for _, name := range []string{"agent.core.tasks.get", "agent.core.tasks.list", "agent.core.tasks.cancel", "agent.core.tasks.retry", "agent.core.tasks.events"} {
		h[name] = m.taskHandler(name)
	}
	for _, name := range []string{"agent.core.schedules.create", "agent.core.schedules.get", "agent.core.schedules.list", "agent.core.schedules.update", "agent.core.schedules.pause", "agent.core.schedules.resume", "agent.core.schedules.trigger", "agent.core.schedules.delete"} {
		h[name] = m.scheduleHandler(name)
	}
	for _, name := range []string{"agent.core.confirmations.get", "agent.core.confirmations.list", "agent.core.confirmations.confirm", "agent.core.confirmations.reject", "agent.core.confirmations.acknowledge_extension_execution_uncertain"} {
		h[name] = m.confirmationHandler(name)
	}
	for _, name := range []string{"agent.core.mcp.discover", "agent.core.mcp.get", "agent.core.mcp.list", "agent.core.mcp.inspect", "agent.core.mcp.install", "agent.core.mcp.update", "agent.core.mcp.remove", "agent.core.mcp.list_tools", "agent.core.mcp.execute"} {
		h[name] = m.delegated(m.cfg.MCP, name, "mcp")
	}
	for _, name := range []string{"agent.core.skills.discover", "agent.core.skills.get", "agent.core.skills.list", "agent.core.skills.inspect", "agent.core.skills.install", "agent.core.skills.update", "agent.core.skills.remove", "agent.core.skills.execute"} {
		// Local skill installation/execution is forbidden in the embedded
		// process, even if a caller accidentally supplies a port.
		h[name] = unavailable
	}
	for _, name := range []string{"agent.core.aws.credentials.create", "agent.core.aws.credentials.update", "agent.core.aws.credentials.delete", "agent.core.aws.credentials.list", "agent.core.aws.credentials.test", "agent.core.aws.plans.get", "agent.core.aws.plans.list", "agent.core.aws.plans.quote", "agent.core.aws.changes.get", "agent.core.aws.changes.list", "agent.core.aws.changes.status", "agent.core.aws.ec2_provisions.plan", "agent.core.aws.ec2_provisions.get", "agent.core.aws.ec2_provisions.list", "agent.core.aws.ec2_provisions.events", "agent.core.aws.ec2_provisions.create.request", "agent.core.aws.ec2_provisions.destroy.request", "agent.core.aws.ec2_provisions.retry"} {
		h[name] = m.delegated(m.cfg.AWS, name, "aws.control")
	}
	for _, name := range []string{"agent.core.aws.ec2_provisions.geolibre_install.plan", "agent.core.aws.ec2_provisions.geolibre_install.request"} {
		h[name] = m.delegatedGeoLibre(name)
	}
	for _, name := range []string{"agent.core.workloads.plan", "agent.core.workloads.get", "agent.core.workloads.list", "agent.core.workloads.quote", "agent.core.workloads.apply", "agent.core.workloads.destroy", "agent.core.workloads.operations.get", "agent.core.workloads.operations.events", "agent.core.workloads.operations.reconcile", "agent.core.workloads.actual.get"} {
		h[name] = m.delegatedWorkload(name)
	}
	for _, name := range []string{"agent.core.dashboard.get", "agent.core.deployments.list", "agent.core.deployments.get", "agent.core.deployments.events"} {
		h[name] = m.deploymentHandler(name)
	}
	return h
}

func (m *Module) delegatedGeoLibre(action string) actionbase.Handler {
	return func(ctx context.Context, params map[string]any) (any, *actionbase.Error) {
		if _, e := m.requireCapability(ctx, params, "aws.control", m.cfg.GeoLibre != nil); e != nil {
			return nil, e
		}
		if _, e := m.requireCapability(ctx, params, "workload.aws_ssm", m.cfg.GeoLibre != nil); e != nil {
			return nil, e
		}
		return m.cfg.GeoLibre.Handle(ctx, m.owner(), action, params)
	}
}

func (m *Module) delegatedWorkload(action string) actionbase.Handler {
	return func(ctx context.Context, params map[string]any) (any, *actionbase.Error) {
		// Keep the public plan seam fail-closed before capability resolution. Raw
		// EC2 SSM requests must use the typed provision/install workflow even when
		// the embedded workload runtime is not ready.
		if action == "agent.core.workloads.plan" {
			if _, ae := rejectRawSSMWorkloadPlan(params); ae != nil {
				return nil, ae
			}
		}
		if target, ok := params["target_kind"].(string); ok && strings.EqualFold(strings.ReplaceAll(target, "-", "_"), "core_runner") {
			return nil, actionbase.BadRequest("CORE_RUNNER workload targets are not supported")
		}
		if target, ok := params["kind"].(string); ok && strings.EqualFold(strings.ReplaceAll(target, "-", "_"), "core_runner") {
			return nil, actionbase.BadRequest("CORE_RUNNER workload targets are not supported")
		}
		token := "workload.aws_ssm"
		hasTarget := false
		if target, ok := params["target_kind"].(string); ok {
			hasTarget = true
			if strings.Contains(strings.ToUpper(strings.ReplaceAll(target, "-", "_")), "ECS") {
				token = "workload.aws_ecs"
			}
		}
		if !hasTarget {
			if !m.capabilityReady("workload.aws_ssm", m.cfg.Workloads != nil) && !m.capabilityReady("workload.aws_ecs", m.cfg.Workloads != nil) {
				return unavailable(ctx, params)
			}
		} else if _, e := m.requireCapability(ctx, params, token, m.cfg.Workloads != nil); e != nil {
			return nil, e
		}
		port := m.cfg.Workloads
		if port == nil {
			return unavailable(ctx, params)
		}
		result, actionErr := port.Handle(ctx, m.owner(), action, params)
		if actionErr != nil {
			return nil, actionErr
		}
		return result, nil
	}
}
