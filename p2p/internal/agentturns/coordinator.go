package agentturns

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
)

type Coordinator struct {
	store Store

	acceptMu sync.Mutex
	mu       sync.Mutex
	turns    map[string]*liveTurn
	queues   map[string][]*turnJob
	running  map[string]bool
}

type liveTurn struct {
	mu          sync.Mutex
	subscribers map[uint64]chan Event
	nextSub     uint64
	cancel      context.CancelFunc
}

type turnJob struct {
	request Request
	run     Runner
	live    *liveTurn
}

func NewCoordinator(ctx context.Context, store Store) (*Coordinator, error) {
	if store == nil {
		return nil, fmt.Errorf("agent turn store is unavailable")
	}
	if _, err := store.InterruptAgentTurns(context.WithoutCancel(ctx)); err != nil {
		return nil, fmt.Errorf("interrupt unsafe agent turns: %w", err)
	}
	return &Coordinator{
		store: store, turns: map[string]*liveTurn{}, queues: map[string][]*turnJob{}, running: map[string]bool{},
	}, nil
}

func (c *Coordinator) Stream(attachmentCtx context.Context, request Request, run Runner, emit Emitter) error {
	if c == nil || c.store == nil {
		return fmt.Errorf("agent turn coordinator is unavailable")
	}
	if err := request.Validate(); err != nil {
		return err
	}
	if run == nil || emit == nil {
		return fmt.Errorf("agent turn runner and emitter are required")
	}

	c.acceptMu.Lock()
	reservation, err := c.store.ReserveAgentTurn(context.Background(), Candidate{
		OwnerID: request.OwnerID, TurnID: request.TurnID, ConversationID: request.ConversationID,
		Action: request.Action, Digest: request.Digest,
	})
	if err != nil {
		c.acceptMu.Unlock()
		return err
	}
	if !bytes.Equal(reservation.Turn.Digest[:], request.Digest[:]) || reservation.Turn.ConversationID != request.ConversationID || reservation.Turn.Action != request.Action {
		c.acceptMu.Unlock()
		return ErrTurnIDReused
	}
	live := c.liveFor(reservation.Turn, reservation.Created)
	events, liveEvents, unsubscribe, err := c.subscribe(reservation.Turn, live, request.AfterSeq)
	if err != nil {
		if reservation.Created {
			c.enqueue(&turnJob{request: request, run: run, live: live})
		}
		c.acceptMu.Unlock()
		return err
	}
	if err := emit(StreamEvent{Kind: EventAccepted, Turn: reservation.Turn, TurnID: reservation.Turn.TurnID, ConversationID: reservation.Turn.ConversationID}); err != nil {
		unsubscribe()
		if reservation.Created {
			c.enqueue(&turnJob{request: request, run: run, live: live})
		}
		c.acceptMu.Unlock()
		return err
	}
	if reservation.Created {
		c.enqueue(&turnJob{request: request, run: run, live: live})
	}
	c.acceptMu.Unlock()

	for _, event := range events {
		if err := emit(streamEvent(event)); err != nil {
			unsubscribe()
			return err
		}
	}
	if reservation.Turn.State.Terminal() || eventsContainTerminal(events) {
		unsubscribe()
		return nil
	}
	defer unsubscribe()
	for {
		select {
		case <-attachmentCtx.Done():
			return attachmentCtx.Err()
		case event, ok := <-liveEvents:
			if !ok {
				return nil
			}
			if err := emit(streamEvent(event)); err != nil {
				return err
			}
			if event.Event == "done" || event.Event == "failed" || event.Event == "stopped" || event.Event == "interrupted" {
				return nil
			}
		}
	}
}

func (c *Coordinator) Stop(ctx context.Context, ownerID, turnID string) (Turn, bool, error) {
	turn, event, changed, err := c.store.StopAgentTurn(context.WithoutCancel(ctx), strings.TrimSpace(ownerID), strings.TrimSpace(turnID))
	if err != nil {
		return Turn{}, false, err
	}
	if !changed {
		return turn, false, nil
	}
	if live := c.lookupLive(turn); live != nil {
		live.mu.Lock()
		cancel := live.cancel
		live.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		c.publish(live, event, true)
	}
	return turn, true, nil
}

func (c *Coordinator) List(ctx context.Context, ownerID, conversationID string, limit int) ([]Turn, error) {
	return c.store.ListAgentTurns(ctx, strings.TrimSpace(ownerID), strings.TrimSpace(conversationID), limit)
}

// LatestConversationRevision returns the newest durable revision acknowledged
// by a successful turn. The Message Server serializes turns per conversation,
// so this value can repair a client cache that missed the terminal done event.
func (c *Coordinator) LatestConversationRevision(ctx context.Context, ownerID, conversationID string) (int64, error) {
	ownerID = strings.TrimSpace(ownerID)
	conversationID = strings.TrimSpace(conversationID)
	if c == nil || c.store == nil || !ValidID(ownerID) || !ValidID(conversationID) {
		return 0, fmt.Errorf("agent conversation revision lookup is invalid")
	}
	event, found, err := c.store.LatestAgentConversationDone(ctx, ownerID, conversationID)
	if err != nil {
		return 0, err
	}
	if !found {
		return 0, nil
	}
	if revision, ok := conversationRevision(event.Data["conversation_revision"]); ok {
		return revision, nil
	}
	return 0, nil
}

func (c *Coordinator) liveFor(turn Turn, created bool) *liveTurn {
	key := turnKey(turn.OwnerID, turn.TurnID)
	c.mu.Lock()
	defer c.mu.Unlock()
	if live := c.turns[key]; live != nil {
		return live
	}
	if !created && turn.State.Terminal() {
		return nil
	}
	live := &liveTurn{subscribers: map[uint64]chan Event{}}
	c.turns[key] = live
	return live
}

func (c *Coordinator) lookupLive(turn Turn) *liveTurn {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.turns[turnKey(turn.OwnerID, turn.TurnID)]
}

func (c *Coordinator) subscribe(turn Turn, live *liveTurn, afterSeq int64) ([]Event, <-chan Event, func(), error) {
	if live == nil {
		events, err := c.store.ListAgentTurnEvents(context.Background(), turn.OwnerID, turn.TurnID, afterSeq)
		return events, nil, func() {}, err
	}
	live.mu.Lock()
	defer live.mu.Unlock()
	events, err := c.store.ListAgentTurnEvents(context.Background(), turn.OwnerID, turn.TurnID, afterSeq)
	if err != nil {
		return nil, nil, func() {}, err
	}
	if turn.State.Terminal() {
		return events, nil, func() {}, nil
	}
	live.nextSub++
	id := live.nextSub
	ch := make(chan Event, 1024)
	live.subscribers[id] = ch
	var once sync.Once
	unsubscribe := func() {
		once.Do(func() {
			live.mu.Lock()
			if existing, ok := live.subscribers[id]; ok {
				delete(live.subscribers, id)
				close(existing)
			}
			live.mu.Unlock()
		})
	}
	return events, ch, unsubscribe, nil
}

func (c *Coordinator) enqueue(job *turnJob) {
	queueKey := conversationKey(job.request.OwnerID, job.request.ConversationID)
	c.mu.Lock()
	c.queues[queueKey] = append(c.queues[queueKey], job)
	if c.running[queueKey] {
		c.mu.Unlock()
		return
	}
	c.running[queueKey] = true
	c.mu.Unlock()
	go c.runQueue(queueKey)
}

func (c *Coordinator) runQueue(queueKey string) {
	for {
		c.mu.Lock()
		queue := c.queues[queueKey]
		if len(queue) == 0 {
			delete(c.queues, queueKey)
			delete(c.running, queueKey)
			c.mu.Unlock()
			return
		}
		job := queue[0]
		c.queues[queueKey] = queue[1:]
		c.mu.Unlock()
		c.runJob(job)
	}
}

func (c *Coordinator) runJob(job *turnJob) {
	turn, changed, err := c.store.MarkAgentTurnRunning(context.Background(), job.request.OwnerID, job.request.TurnID)
	if err != nil {
		c.closeLive(job.live)
		return
	}
	if !changed {
		return
	}
	executionCtx, cancel := context.WithCancel(context.Background())
	job.live.mu.Lock()
	job.live.cancel = cancel
	job.live.mu.Unlock()
	defer func() {
		cancel()
		job.live.mu.Lock()
		job.live.cancel = nil
		job.live.mu.Unlock()
		c.mu.Lock()
		delete(c.turns, turnKey(job.request.OwnerID, job.request.TurnID))
		c.mu.Unlock()
	}()

	done := false
	emit := func(runtimeEvent RuntimeEvent) error {
		name := strings.TrimSpace(runtimeEvent.Event)
		if name == "" {
			name = "message"
		}
		data := SanitizeData(runtimeEvent.Data)
		if name == "done" {
			_, event, transitioned, err := c.store.FinishAgentTurn(context.Background(), turn.OwnerID, turn.TurnID, StateSucceeded, "runtime", "done", data, "")
			if err != nil {
				return err
			}
			if transitioned {
				done = true
				c.publish(job.live, event, true)
			}
			return nil
		}
		if name == "error" {
			_, event, transitioned, err := c.store.FinishAgentTurn(
				context.Background(), turn.OwnerID, turn.TurnID, StateFailed,
				EventError, "failed", data, "native agent turn failed",
			)
			if err != nil {
				return err
			}
			if transitioned {
				c.publish(job.live, event, true)
			}
			return nil
		}
		event, err := c.store.AppendAgentTurnEvent(context.Background(), turn.OwnerID, turn.TurnID, "runtime", name, data)
		if err != nil {
			return err
		}
		if event.Seq > 0 {
			c.publish(job.live, event, false)
		}
		return nil
	}
	err = job.run(executionCtx, emit)
	if done {
		return
	}
	current, ok, loadErr := c.store.GetAgentTurn(context.Background(), turn.OwnerID, turn.TurnID)
	if loadErr == nil && ok && current.State.Terminal() {
		return
	}
	if err != nil {
		message, data := publicRunnerFailure(err)
		_, event, transitioned, finishErr := c.store.FinishAgentTurn(context.Background(), turn.OwnerID, turn.TurnID, StateFailed, "error", "failed", data, message)
		if finishErr == nil && transitioned {
			c.publish(job.live, event, true)
		} else if finishErr != nil {
			c.closeLive(job.live)
		}
		return
	}
	_, event, transitioned, finishErr := c.store.FinishAgentTurn(context.Background(), turn.OwnerID, turn.TurnID, StateSucceeded, "runtime", "done", map[string]any{}, "")
	if finishErr == nil && transitioned {
		c.publish(job.live, event, true)
	} else if finishErr != nil {
		c.closeLive(job.live)
	}
}

type codedRunnerFailure interface {
	error
	ErrorCode() string
}

func publicRunnerFailure(err error) (string, map[string]any) {
	message := "native agent turn failed"
	data := map[string]any{"error": message}
	var coded codedRunnerFailure
	if !errors.As(err, &coded) {
		return message, data
	}
	code := strings.TrimSpace(coded.ErrorCode())
	publicMessage := strings.TrimSpace(coded.Error())
	if !strings.HasPrefix(code, "M_AGENT_") || publicMessage == "" || len(publicMessage) > 256 {
		return message, data
	}
	data["error"] = publicMessage
	data["error_code"] = code
	return publicMessage, data
}

func conversationRevision(value any) (int64, bool) {
	switch revision := value.(type) {
	case int:
		if revision >= 0 {
			return int64(revision), true
		}
	case int64:
		if revision >= 0 {
			return revision, true
		}
	case float64:
		if revision >= 0 && revision <= math.MaxInt64 && math.Trunc(revision) == revision {
			return int64(revision), true
		}
	case json.Number:
		parsed, err := revision.Int64()
		if err == nil && parsed >= 0 {
			return parsed, true
		}
	}
	return 0, false
}

func (c *Coordinator) publish(live *liveTurn, event Event, terminal bool) {
	if live == nil {
		return
	}
	live.mu.Lock()
	defer live.mu.Unlock()
	for id, subscriber := range live.subscribers {
		select {
		case subscriber <- event:
		default:
			close(subscriber)
			delete(live.subscribers, id)
		}
	}
	if terminal {
		for id, subscriber := range live.subscribers {
			close(subscriber)
			delete(live.subscribers, id)
		}
	}
}

func (c *Coordinator) closeLive(live *liveTurn) {
	if live == nil {
		return
	}
	live.mu.Lock()
	defer live.mu.Unlock()
	for id, subscriber := range live.subscribers {
		close(subscriber)
		delete(live.subscribers, id)
	}
}

func streamEvent(event Event) StreamEvent {
	return StreamEvent{
		Kind: event.Kind, TurnID: event.TurnID, ConversationID: event.ConversationID,
		Seq: event.Seq, Event: event.Event, Data: event.Data,
	}
}

func eventsContainTerminal(events []Event) bool {
	if len(events) == 0 {
		return false
	}
	switch events[len(events)-1].Event {
	case "done", "failed", "stopped", "interrupted":
		return true
	default:
		return false
	}
}

func turnKey(ownerID, turnID string) string                 { return ownerID + "\x00" + turnID }
func conversationKey(ownerID, conversationID string) string { return ownerID + "\x00" + conversationID }
