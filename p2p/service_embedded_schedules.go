package p2p

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	p2pstorage "github.com/YingSuiAI/dirextalk-message-server/p2p/storage"
	"github.com/YingSuiAI/dirextalk-message-server/setup/process"
	"github.com/sirupsen/logrus"
)

const (
	defaultAgentConfirmationSweepInterval = time.Second
	agentConfirmationSweepTimeout         = 2 * time.Second
	maxAgentConfirmationSweepBackoff      = 30 * time.Second
)

// runAgentConfirmationSweep keeps approval expiry independent from client
// reads while remaining owned by the process lifecycle. Errors are retried
// with bounded backoff; a failed sweep must not stop the worker or schedule
// loop.
func runAgentConfirmationSweep(ctx context.Context, interval time.Duration, owner func() string, sweep func(context.Context, string, time.Time) error, observe func(error, bool)) {
	if ctx == nil || owner == nil || sweep == nil {
		return
	}
	if interval <= 0 {
		interval = defaultAgentConfirmationSweepInterval
	}
	if interval > maxAgentConfirmationSweepBackoff {
		interval = maxAgentConfirmationSweepBackoff
	}
	timer := time.NewTimer(interval)
	defer timer.Stop()
	backoff := interval
	failed := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}

		ownerID := strings.TrimSpace(owner())
		if ownerID == "" {
			backoff = interval
			timer.Reset(backoff)
			continue
		}
		sweepCtx, cancel := context.WithTimeout(ctx, agentConfirmationSweepTimeout)
		err := sweep(sweepCtx, ownerID, time.Now().UTC())
		cancel()
		if err != nil {
			if ctx.Err() == nil && observe != nil {
				observe(err, false)
			}
			failed = true
			if backoff >= maxAgentConfirmationSweepBackoff/2 {
				backoff = maxAgentConfirmationSweepBackoff
			} else {
				backoff *= 2
			}
		} else {
			if failed && observe != nil {
				observe(nil, true)
			}
			failed = false
			backoff = interval
		}
		timer.Reset(backoff)
	}
}

func observeAgentConfirmationSweep(err error, recovered bool) {
	if recovered {
		logrus.WithField("component", "agent_confirmation_sweeper").Info("embedded Agent confirmation expiry sweep recovered")
		return
	}
	logrus.WithError(err).WithField("component", "agent_confirmation_sweeper").Warn("embedded Agent confirmation expiry sweep failed")
}

func startAgentConfirmationSweeper(processCtx *process.ProcessContext, owner func() string, sweep func(context.Context, string, time.Time) error, interval time.Duration) {
	if processCtx == nil || owner == nil || sweep == nil {
		return
	}
	processCtx.ComponentStarted()
	go func() {
		defer processCtx.ComponentFinished()
		runAgentConfirmationSweep(processCtx.Context(), interval, owner, sweep, observeAgentConfirmationSweep)
	}()
}

// EmbeddedSchedulesReady is fail-closed and intentionally distinct from the
// Agent Core capability. It is true only when durable storage, pinned profile
// resolution and the restricted runner are all wired.
func (s *Service) EmbeddedSchedulesReady() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.agentSecretRuntimeReadyLocked() &&
		s.scheduleModule != nil &&
		s.scheduleModule.Ready() &&
		s.agentTaskRuntime != nil &&
		s.agentScheduleLoop != nil &&
		s.scheduleRunning
}

func (s *Service) agentSecretRuntimeReadyLocked() bool {
	return s.agentSecretReady &&
		s.modelProfiles != nil &&
		s.modelProfiles.ModelProfileStoreReady()
}

func (s *Service) closeAgentSecretGuard() {
	if s == nil {
		return
	}
	s.agentSecretGuardCloseOnce.Do(func() {
		s.mu.Lock()
		guard := s.agentSecretGuard
		s.agentSecretGuard = nil
		s.mu.Unlock()
		_ = guard.Close()
	})
}

func (s *Service) registerAgentSecretGuardShutdown(processCtx *process.ProcessContext) {
	if s == nil || processCtx == nil {
		return
	}
	processCtx.RegisterShutdownCallback(s.closeAgentSecretGuard)
}

func (s *Service) embeddedAgentCapabilityReady(capability string) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch strings.TrimSpace(capability) {
	case "model_profiles.server", "model_roles.server":
		return s.agentSecretRuntimeReadyLocked()
	case "memory.server":
		_, ok := s.store.(*p2pstorage.DatabaseStore)
		return ok
	case "voice.server":
		return s.agentModule != nil && s.agentModule.VoiceReady()
	case "task", "confirmation", "schedules.server":
		return s.agentSecretRuntimeReadyLocked() &&
			s.scheduleRunning &&
			s.agentTaskRuntime != nil &&
			s.agentScheduleLoop != nil
	case "mcp", "aws.control", "workload.aws_ssm", "workload.aws_ecs":
		return s.agentSecretRuntimeReadyLocked() &&
			s.scheduleRunning &&
			s.agentTaskRuntime != nil &&
			s.agentTaskExecutor != nil &&
			s.agentTaskExecutor.ready(capability)
	case "deployments.server":
		if !s.deploymentSourceReady {
			return false
		}
		dbStore, ok := s.store.(*p2pstorage.DatabaseStore)
		return ok && unifiedDeploymentSchemaReady(context.Background(), dbStore.DB())
	default:
		return false
	}
}

// unifiedDeploymentSchemaReady probes the v106 tables instead of relying on a
// store method set. Both DatabaseStore and MemoryStore retain legacy ledger
// methods for compatibility, so a type assertion alone would accidentally
// publish deployments.server against the retired in-memory/legacy source.
func unifiedDeploymentSchemaReady(ctx context.Context, db *sql.DB) bool {
	if db == nil {
		return false
	}
	probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	var ready bool
	err := db.QueryRowContext(probeCtx, `
		SELECT to_regclass('core_deployments') IS NOT NULL
		   AND to_regclass('core_deployment_event_counters') IS NOT NULL
		   AND to_regclass('core_deployment_events') IS NOT NULL`).Scan(&ready)
	return err == nil && ready
}

// StartEmbeddedScheduler starts the single generic schedule loop and task
// worker. Shutdown stops claiming first, waits a bounded grace period for
// in-flight calls, then cancels them so their leases can be reclaimed.
func (s *Service) StartEmbeddedScheduler(processCtx *process.ProcessContext, workerID string) bool {
	if s == nil || processCtx == nil {
		return false
	}
	s.mu.Lock()
	if s.agentRuntimeStarted {
		ready := s.scheduleRunning
		s.mu.Unlock()
		return ready
	}
	s.agentRuntimeStarted = true
	worker := s.agentTaskRuntime
	loop := s.agentScheduleLoop
	confirmationSweep := s.agentConfirmationSweep
	confirmationSweepInterval := s.agentConfirmationSweepInterval
	owner := s.OwnerMXID
	ready := s.agentSecretRuntimeReadyLocked() &&
		s.scheduleModule != nil &&
		s.scheduleModule.Ready() &&
		worker != nil &&
		loop != nil &&
		s.agentRuntimeInitErr == nil
	s.mu.Unlock()
	s.registerAgentSecretGuardShutdown(processCtx)
	startAgentConfirmationSweeper(processCtx, owner, confirmationSweep, confirmationSweepInterval)
	if !ready {
		return false
	}
	s.mu.Lock()
	s.scheduleRunning = true
	s.mu.Unlock()

	_ = strings.TrimSpace(workerID) // Worker holder is fixed at construction.
	processCtx.ComponentStarted()
	go func() {
		defer processCtx.ComponentFinished()
		defer func() {
			s.mu.Lock()
			s.scheduleRunning = false
			s.mu.Unlock()
		}()
		workerCtx, cancelWorker := context.WithCancel(context.Background())
		scheduleCtx, cancelSchedule := context.WithCancel(context.Background())
		defer cancelWorker()
		defer cancelSchedule()
		workerDone := make(chan error, 1)
		scheduleDone := make(chan error, 1)
		go func() { workerDone <- worker.Run(workerCtx) }()
		go func() { scheduleDone <- loop.Run(scheduleCtx) }()

		var runtimeErr error
		select {
		case <-processCtx.Context().Done():
		case runtimeErr = <-workerDone:
			if runtimeErr != nil {
				runtimeErr = fmt.Errorf("embedded Agent task worker stopped: %w", runtimeErr)
			}
		case runtimeErr = <-scheduleDone:
			if runtimeErr != nil {
				runtimeErr = fmt.Errorf("embedded Agent schedule loop stopped: %w", runtimeErr)
			}
		}

		cancelSchedule()
		grace, cancelGrace := context.WithTimeout(context.Background(), 30*time.Second)
		stopErr := worker.StopWithContext(grace)
		cancelGrace()
		cancelWorker()
		if runtimeErr != nil {
			processCtx.Degraded(runtimeErr)
		}
		if stopErr != nil && processCtx.Context().Err() == nil {
			processCtx.Degraded(fmt.Errorf("embedded Agent worker shutdown: %w", stopErr))
		}
	}()
	return true
}
