package agent

import (
	"context"
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hzj629206/assistant/cache"
)

const defaultQueueSize = 128
const defaultWorkerCount = 4
const defaultNonTextMergeWindow = 10 * time.Second
const defaultSessionIdleTimeout = 10 * time.Minute
const defaultShutdownTurnTimeout = 15 * time.Second
const defaultShutdownInterruptTimeout = 1000 * time.Millisecond
const groupConversationMentionTimeout = time.Hour

// ErrDispatcherClosed indicates the dispatcher is no longer accepting new work.
var ErrDispatcherClosed = errors.New("dispatcher is closed")
var errRunnerNotConfigured = errors.New("runner is not configured")

// DispatcherOptions configures the asynchronous callback dispatcher.
type DispatcherOptions struct {
	Store               *ConversationStore
	Runner              Runner
	FatalErrCh          chan<- error
	QueueSize           int
	WorkerCount         int
	NonTextMergeWindow  time.Duration
	SessionIdleTimeout  time.Duration
	ShutdownTurnTimeout time.Duration
}

// Dispatcher normalizes callback events and runs them asynchronously.
type Dispatcher struct {
	store                    *ConversationStore
	runner                   Runner
	queue                    *dispatcherQueue
	workerCount              int
	mergeWindow              time.Duration
	locks                    *keyedLocker
	pendingMu                sync.Mutex
	pending                  map[string]*pendingConversation
	delayed                  map[string]*delayedConversation
	commandMu                sync.Mutex
	commanding               map[string]*commandConversation
	commandWG                sync.WaitGroup
	startOnce                sync.Once
	stopOnce                 sync.Once
	sessionsCloseOnce        sync.Once
	workersDone              chan struct{}
	workersWG                sync.WaitGroup
	closeMu                  sync.RWMutex
	closed                   bool
	stopCh                   chan struct{}
	fatalErrCh               chan<- error
	sessionIdleTimeout       time.Duration
	shutdownTurnTimeout      time.Duration
	sessionsMu               sync.Mutex
	sessions                 map[string]*managedConversationSession
	activeWorkMu             sync.Mutex
	activeWorkSeq            uint64
	activeWorkByConversation map[string]*activeConversationWork
}

type pendingConversation struct {
	active bool
	queued bool
	batch  []InboundMessage
}

type delayedConversation struct {
	batch      []InboundMessage
	generation uint64
}

type commandConversation struct {
	queue         []CommandRequest
	briefs        []string
	blockingCount int
	running       bool
}

type activeConversationWork struct {
	id           uint64
	conversation ConversationState
	mu           sync.Mutex
	session      Session
	turn         Turn
	turnDone     chan struct{}
	sessionOnce  sync.Once
	done         chan struct{}
	interrupted  atomic.Bool
}

type managedConversationSession struct {
	session        Session
	idleTimer      *time.Timer
	inUse          int
	idleGeneration uint64
}

type dispatcherQueue struct {
	mu       sync.Mutex
	items    []InboundMessage
	capacity int
	readyCh  chan struct{}
	spaceCh  chan struct{}
}

func newDispatcherQueue(capacity int) *dispatcherQueue {
	if capacity <= 0 {
		capacity = defaultQueueSize
	}

	return &dispatcherQueue{
		items:    make([]InboundMessage, 0, capacity),
		capacity: capacity,
		readyCh:  make(chan struct{}, 1),
		spaceCh:  make(chan struct{}, 1),
	}
}

func (q *dispatcherQueue) Capacity() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.capacity
}

func (q *dispatcherQueue) Enqueue(ctx context.Context, stopCh <-chan struct{}, message InboundMessage) (int, error) {
	for {
		q.mu.Lock()
		if len(q.items) < q.capacity {
			q.items = append(q.items, message)
			queueLen := len(q.items)
			q.mu.Unlock()
			q.signalReady()
			return queueLen, nil
		}
		q.mu.Unlock()

		select {
		case <-stopCh:
			return 0, ErrDispatcherClosed
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-q.spaceCh:
		}
	}
}

func (q *dispatcherQueue) ClaimNext(stopCh <-chan struct{}, claim func(InboundMessage)) (InboundMessage, bool) {
	for {
		q.mu.Lock()
		if len(q.items) > 0 {
			message := q.items[0]
			if claim != nil {
				claim(message)
			}
			copy(q.items, q.items[1:])
			q.items = q.items[:len(q.items)-1]
			remaining := len(q.items)
			q.mu.Unlock()
			q.signalSpace()
			if remaining > 0 {
				q.signalReady()
			}
			return message, true
		}
		q.mu.Unlock()

		select {
		case <-stopCh:
			return InboundMessage{}, false
		case <-q.readyCh:
		}
	}
}

func (q *dispatcherQueue) RemoveMatching(match func(InboundMessage) bool) []InboundMessage {
	q.mu.Lock()
	wasFull := len(q.items) >= q.capacity
	kept := q.items[:0]
	removed := make([]InboundMessage, 0)
	for _, item := range q.items {
		if match(item) {
			removed = append(removed, item)
			continue
		}
		kept = append(kept, item)
	}
	q.items = kept
	remaining := len(q.items)
	q.mu.Unlock()

	if wasFull && remaining < q.capacity {
		q.signalSpace()
	}
	if remaining > 0 {
		q.signalReady()
	}
	return removed
}

func (q *dispatcherQueue) Drain() []InboundMessage {
	q.mu.Lock()
	drained := append([]InboundMessage(nil), q.items...)
	wasFull := len(q.items) >= q.capacity
	q.items = q.items[:0]
	q.mu.Unlock()

	if wasFull {
		q.signalSpace()
	}
	return drained
}

func (q *dispatcherQueue) TryDequeue() (InboundMessage, bool) {
	q.mu.Lock()
	if len(q.items) == 0 {
		q.mu.Unlock()
		return InboundMessage{}, false
	}
	message := q.items[0]
	copy(q.items, q.items[1:])
	q.items = q.items[:len(q.items)-1]
	remaining := len(q.items)
	wasFull := remaining+1 >= q.capacity
	q.mu.Unlock()

	if wasFull {
		q.signalSpace()
	}
	if remaining > 0 {
		q.signalReady()
	}
	return message, true
}

func (q *dispatcherQueue) signalReady() {
	select {
	case q.readyCh <- struct{}{}:
	default:
	}
}

func (q *dispatcherQueue) signalSpace() {
	select {
	case q.spaceCh <- struct{}{}:
	default:
	}
}

// NewDispatcher builds a dispatcher with in-memory queueing.
func NewDispatcher(options DispatcherOptions) *Dispatcher {
	queueSize := options.QueueSize
	if queueSize <= 0 {
		queueSize = defaultQueueSize
	}

	workerCount := options.WorkerCount
	if workerCount <= 0 {
		workerCount = defaultWorkerCount
	}

	store := options.Store
	if store == nil {
		store = NewConversationStore(cache.Global())
	}

	mergeWindow := options.NonTextMergeWindow
	if mergeWindow <= 0 {
		mergeWindow = defaultNonTextMergeWindow
	}

	runner := options.Runner
	if runner == nil {
		runner = missingRunner{}
	}

	return &Dispatcher{
		store:                    store,
		runner:                   runner,
		queue:                    newDispatcherQueue(queueSize),
		workerCount:              workerCount,
		mergeWindow:              mergeWindow,
		locks:                    newKeyedLocker(),
		pending:                  make(map[string]*pendingConversation),
		delayed:                  make(map[string]*delayedConversation),
		commanding:               make(map[string]*commandConversation),
		workersDone:              make(chan struct{}),
		stopCh:                   make(chan struct{}),
		fatalErrCh:               options.FatalErrCh,
		sessionIdleTimeout:       normalizedDispatcherSessionIdleTimeout(options.SessionIdleTimeout),
		shutdownTurnTimeout:      options.ShutdownTurnTimeout,
		sessions:                 make(map[string]*managedConversationSession),
		activeWorkByConversation: make(map[string]*activeConversationWork),
	}
}

func normalizedDispatcherSessionIdleTimeout(timeout time.Duration) time.Duration {
	if timeout <= 0 {
		return defaultSessionIdleTimeout
	}
	return timeout
}

type missingRunner struct{}

func (missingRunner) StartSession(context.Context, SessionOptions) (Session, error) {
	return nil, errRunnerNotConfigured
}

func (missingRunner) Close() error { return nil }

func (missingRunner) RegisterSystemPrompt(string) {}

func (missingRunner) RegisterTools(...Tool) {}

// Start launches background workers.
func (d *Dispatcher) Start() error {
	d.startOnce.Do(func() {
		log.Printf("dispatcher starting workers: worker_count=%d queue_size=%d", d.workerCount, d.queue.Capacity())
		for workerID := 0; workerID < d.workerCount; workerID++ {
			d.workersWG.Add(1)
			go func(id int) {
				defer d.workersWG.Done()
				d.runWorker(id)
			}(workerID)
		}

		go func() {
			d.workersWG.Wait()
			close(d.workersDone)
		}()
	})

	return nil
}

// Shutdown stops accepting new work, drops queued-but-not-running work, and waits for running work.
func (d *Dispatcher) Shutdown(ctx context.Context) error {
	d.initiateShutdown()
	droppedMessages, cleanupErr := d.dropQueuedWork(context.Background())            //nolint:contextcheck
	droppedCommands, commandCleanupErr := d.dropQueuedCommands(context.Background()) //nolint:contextcheck
	log.Printf("dispatcher shutdown requested")
	if droppedMessages > 0 {
		log.Printf("dispatcher dropped queued work during shutdown: message_count=%d", droppedMessages)
	}
	if droppedCommands > 0 {
		log.Printf("dispatcher dropped queued commands during shutdown: command_count=%d", droppedCommands)
	}

	workersStopped, waitErr := d.waitForWorkers(ctx)
	var closeErr error
	if workersStopped {
		closeErr = d.closeSessionsOnce()
	} else {
		d.closeSessionsAfterWorkers()
	}
	shutdownErr := errors.Join(cleanupErr, commandCleanupErr, waitErr, closeErr)
	if shutdownErr != nil {
		return shutdownErr
	}

	log.Printf("dispatcher shutdown completed")
	return nil
}

func (d *Dispatcher) waitForWorkers(ctx context.Context) (bool, error) {
	shutdownTurnTimeout := d.shutdownTurnTimeout
	if shutdownTurnTimeout <= 0 {
		shutdownTurnTimeout = defaultShutdownTurnTimeout
	}

	timer := time.NewTimer(shutdownTurnTimeout)
	defer timer.Stop()

	done := make(chan struct{})
	go func() {
		d.workersWG.Wait()
		d.commandWG.Wait()
		close(done)
	}()

	select {
	case <-done:
		return true, nil
	case <-timer.C:
		log.Printf("dispatcher shutdown grace period elapsed; closing managed sessions")
		if err := d.closeSessionsOnce(); err != nil {
			return false, err
		}
	case <-ctx.Done():
		_ = d.closeSessionsOnce()
		return false, ctx.Err()
	}

	select {
	case <-done:
		return true, nil
	case <-time.After(defaultShutdownInterruptTimeout):
		log.Printf("dispatcher shutdown close grace period elapsed; deferring final cleanup to runner close")
		return false, nil
	case <-ctx.Done():
		_ = d.closeSessionsOnce()
		return false, ctx.Err()
	}
}

// Enqueue adds one inbound message to the asynchronous processing queue.
func (d *Dispatcher) Enqueue(ctx context.Context, message InboundMessage) error {
	if message.ConversationKey == "" {
		return errors.New("enqueue dispatcher message failed: conversation key is empty")
	}

	if d.isClosed() {
		return ErrDispatcherClosed
	}

	if d.tryRecordCommandDrop(message) {
		log.Printf("dispatcher dropped inbound message during command: conversation=%s event_id=%s", message.ConversationKey, message.ID)
		return cleanupInboundMessages(context.WithoutCancel(ctx), flattenInboundMessages(message))
	}

	handled, readyBatch := d.handleDelayedMessage(ctx, message)
	switch {
	case len(readyBatch) > 0:
		message = combineInboundMessages(readyBatch)
	case handled:
		log.Printf("dispatcher delayed message: conversation=%s event_id=%s kind=%s", message.ConversationKey, message.ID, message.Kind)
		return nil
	case d.mergePendingMessage(message):
		log.Printf("dispatcher merged message into pending conversation: conversation=%s event_id=%s", message.ConversationKey, message.ID)
		return nil
	}

	return d.enqueueReadyMessage(ctx, message)
}

func (d *Dispatcher) runWorker(workerID int) {
	log.Printf("dispatcher worker started: worker_id=%d", workerID)
	defer func() {
		if recovered := recover(); recovered != nil {
			d.reportFatalError(fmt.Errorf("dispatcher worker %d panicked: %v", workerID, recovered))
			d.initiateShutdown()
		}
	}()

	for {
		select {
		case <-d.stopCh:
			return
		default:
		}

		message, ok := d.queue.ClaimNext(d.stopCh, d.claimConversationWork)
		if !ok {
			return
		}

		current := message
		for {
			if err := d.handleMessage(context.Background(), current); err != nil && !errors.Is(err, context.Canceled) {
				log.Printf(
					"dispatcher worker %d failed: conversation=%s event_id=%s err=%v",
					workerID,
					current.ConversationKey,
					current.ID,
					err,
				)
			}

			next, ok := d.nextPendingMessage(current.ConversationKey)
			if !ok {
				break
			}
			current = next
		}
	}
}

func (d *Dispatcher) handleMessage(ctx context.Context, message InboundMessage) (err error) {
	if message.ConversationKey == "" {
		return errors.New("handle message failed: conversation key is empty")
	}
	releaseWork := d.releaseClaimedConversationWork(message.ConversationKey)
	defer func() {
		releaseWork()
	}()

	log.Printf("dispatcher handling message: conversation=%s event_id=%s kind=%s", message.ConversationKey, message.ID, message.Kind)

	messages := flattenInboundMessages(message)
	defer func() {
		cleanupErr := cleanupInboundMessages(ctx, messages)
		if cleanupErr == nil {
			return
		}
		if err == nil {
			err = fmt.Errorf("cleanup responder failed: %w", cleanupErr)
			return
		}
		log.Printf("cleanup responder failed: conversation=%s event_id=%s err=%v", message.ConversationKey, message.ID, cleanupErr)
	}()

	freshMessages, err := d.filterProcessedMessages(ctx, messages)
	if err != nil {
		return err
	}
	if len(freshMessages) == 0 {
		log.Printf("dispatcher skipped duplicate batch: conversation=%s event_id=%s", message.ConversationKey, message.ID)
		return nil
	}
	message = combineInboundMessages(freshMessages)
	if d.tryRecordCommandDrop(message) {
		log.Printf("dispatcher dropped command-overridden batch: conversation=%s event_id=%s", message.ConversationKey, message.ID)
		return nil
	}

	unlock := d.locks.Lock(message.ConversationKey)
	defer unlock()
	if d.tryRecordCommandDrop(message) {
		log.Printf("dispatcher dropped command-overridden conversation after lock: conversation=%s event_id=%s", message.ConversationKey, message.ID)
		return nil
	}

	state, err := d.store.GetConversation(ctx, message.ConversationKey)
	isNewConversation := false
	if err != nil {
		if !errors.Is(err, cache.ErrNotFound) {
			return err
		}
		if isGroupConversationKey(message.ConversationKey) && !messagesHaveTag(freshMessages, MessageTagGroupMentioned) {
			log.Printf("dispatcher ignored unmentioned message without conversation state: conversation=%s event_id=%s", message.ConversationKey, message.ID)
			return nil
		}
		isNewConversation = true
		state = newConversationState(message)
		log.Printf("dispatcher created conversation state: conversation=%s event_id=%s", message.ConversationKey, message.ID)
	}
	if state.Muted {
		if !messagesHaveTag(freshMessages, MessageTagGroupMentioned) {
			state.LastEventID = message.ID
			state.LastActivityAt = time.Now()
			if err = d.store.PutConversation(ctx, state); err != nil {
				return err
			}
			log.Printf("dispatcher ignored message for muted conversation: conversation=%s event_id=%s", message.ConversationKey, message.ID)
			return nil
		}

		state.Muted = false
		state.LastEventID = message.ID
		state.LastActivityAt = time.Now()
		if err = d.store.PutConversation(ctx, state); err != nil {
			return err
		}
		log.Printf("dispatcher unmuted mentioned conversation: conversation=%s event_id=%s", message.ConversationKey, message.ID)
	}
	if isGroupConversationKey(message.ConversationKey) &&
		conversationStateInactive(state, time.Now()) &&
		!messagesHaveTag(freshMessages, MessageTagGroupMentioned) {
		log.Printf(
			"dispatcher ignored unmentioned message for inactive conversation: conversation=%s event_id=%s last_activity_at=%s",
			message.ConversationKey,
			message.ID,
			state.LastActivityAt.Format(time.RFC3339),
		)
		return nil
	}
	var activeWork *activeConversationWork
	activeWork, releaseWork = d.startActiveConversationWork(state)

	if isNewConversation && message.LoadInitialContext != nil {
		initialContext, loadErr := message.LoadInitialContext(ctx)
		if loadErr != nil {
			return fmt.Errorf("load initial context failed: %w", loadErr)
		}
		message.initialContext = initialContext
		log.Printf("dispatcher loaded initial context: conversation=%s event_id=%s context_len=%d", message.ConversationKey, message.ID, len(initialContext))
	}
	if isNewConversation && message.LoadInitialMessages != nil {
		initialMessages, loadErr := message.LoadInitialMessages(ctx)
		if loadErr != nil {
			return fmt.Errorf("load initial messages failed: %w", loadErr)
		}
		message = prependHistoricalMessages(message, initialMessages)
		log.Printf(
			"dispatcher loaded initial messages: conversation=%s event_id=%s history_count=%d merged_count=%d",
			message.ConversationKey,
			message.ID,
			len(message.historicalMessages),
			len(message.mergedMessages),
		)
	}

	log.Printf(
		"dispatcher running turn: conversation=%s event_id=%s existing_session=%s",
		message.ConversationKey,
		message.ID,
		state.RunnerThreadID,
	)
	session, releaseSession, err := d.acquireConversationSession(ctx, state)
	if err != nil {
		return fmt.Errorf("start session failed: %w", err)
	}
	defer releaseSession()
	scheduledTurn, err := session.ScheduleTurn(ctx, TurnRequest{
		Conversation: state,
		Message:      message,
	})
	if err != nil {
		return fmt.Errorf("schedule turn failed: %w", err)
	}
	d.finishActiveConversationWork(activeWork, session, scheduledTurn)
	d.persistConversationSessionID(ctx, state, session)
	if activeWork != nil && activeWork.interrupted.Load() {
		log.Printf(
			"dispatcher skipping run turn for interrupted active turn: conversation=%s event_id=%s session_id=%s",
			message.ConversationKey,
			message.ID,
			session.ID(),
		)
		return nil
	}

	result, err := scheduledTurn.Run(ctx)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			state.RunnerThreadID = session.ID()
			putErr := d.store.PutConversation(context.WithoutCancel(ctx), state)
			if putErr != nil {
				log.Printf("dispatcher failed to persist conversation after turn cancellation: conversation=%s session_id=%s err=%v", state.Key, state.RunnerThreadID, putErr)
			} else {
				log.Printf("dispatcher persisted conversation after canceled turn: conversation=%s session_id=%s", state.Key, state.RunnerThreadID)
			}
		} else {
			d.discardConversationSession(state.Key, session)
		}
		return err
	}
	log.Printf(
		"dispatcher completed turn: conversation=%s event_id=%s session_id=%s reply_len=%d",
		message.ConversationKey,
		message.ID,
		session.ID(),
		len(result.ReplyText),
	)
	if activeWork != nil && activeWork.interrupted.Load() {
		log.Printf("dispatcher suppressing reply from interrupted turn: conversation=%s event_id=%s session_id=%s", message.ConversationKey, message.ID, session.ID())
		result.ReplyText = ""
	}

	state.LastEventID = message.ID
	state.LastActivityAt = time.Now()
	if sessionID := strings.TrimSpace(session.ID()); sessionID != "" {
		state.RunnerThreadID = sessionID
	}

	if err := d.store.PutConversation(ctx, state); err != nil {
		return err
	}
	log.Printf("dispatcher stored conversation state: conversation=%s event_id=%s session_id=%s", state.Key, message.ID, state.RunnerThreadID)

	if result.ReplyText == "" {
		log.Printf("dispatcher finished without reply: conversation=%s event_id=%s", message.ConversationKey, message.ID)
		return nil
	}
	if message.Responder == nil {
		return LoggingResponder{}.SendText(ctx, result.ReplyText)
	}

	log.Printf("dispatcher sending reply: conversation=%s event_id=%s", message.ConversationKey, message.ID)
	return message.Responder.SendText(ctx, result.ReplyText)
}

func (d *Dispatcher) mergePendingMessage(message InboundMessage) bool {
	d.pendingMu.Lock()
	defer d.pendingMu.Unlock()

	state := d.pending[message.ConversationKey]
	if state == nil {
		state = &pendingConversation{}
		d.pending[message.ConversationKey] = state
	}
	if !state.active && !state.queued {
		state.queued = true
		return false
	}

	state.batch = append(state.batch, flattenInboundMessages(message)...)
	return true
}

func (d *Dispatcher) handleDelayedMessage(ctx context.Context, message InboundMessage) (bool, []InboundMessage) {
	d.pendingMu.Lock()
	defer d.pendingMu.Unlock()

	state := d.delayed[message.ConversationKey]
	if state == nil {
		if !shouldDelayInboundMessage(message) || d.mergeWindow <= 0 {
			return false, nil
		}

		state = &delayedConversation{
			batch: []InboundMessage{message},
		}
		d.delayed[message.ConversationKey] = state
		d.scheduleDelayedFlushLocked(ctx, message.ConversationKey, state)
		return true, nil
	}

	state.batch = append(state.batch, flattenInboundMessages(message)...)
	if message.Kind == MessageKindText {
		delete(d.delayed, message.ConversationKey)
		if d.appendToPendingLocked(message.ConversationKey, state.batch) {
			return true, nil
		}
		d.markQueuedLocked(message.ConversationKey)
		return true, state.batch
	}

	d.scheduleDelayedFlushLocked(ctx, message.ConversationKey, state)
	return true, nil
}

func (d *Dispatcher) scheduleDelayedFlushLocked(ctx context.Context, conversationKey string, state *delayedConversation) {
	if state == nil {
		return
	}

	state.generation++
	generation := state.generation
	flushCtx := context.WithoutCancel(ctx)
	time.AfterFunc(d.mergeWindow, func() {
		d.flushDelayedConversation(flushCtx, conversationKey, generation)
	})
}

func (d *Dispatcher) flushDelayedConversation(ctx context.Context, conversationKey string, generation uint64) {
	d.pendingMu.Lock()
	state := d.delayed[conversationKey]
	if state == nil || state.generation != generation {
		d.pendingMu.Unlock()
		return
	}

	delete(d.delayed, conversationKey)
	if d.appendToPendingLocked(conversationKey, state.batch) {
		d.pendingMu.Unlock()
		return
	}

	message := combineInboundMessages(state.batch)
	d.markQueuedLocked(conversationKey)
	d.pendingMu.Unlock()

	if err := d.enqueueReadyMessage(ctx, message); err != nil && !errors.Is(err, ErrDispatcherClosed) {
		log.Printf(
			"dispatcher failed to flush delayed conversation: conversation=%s event_id=%s err=%v",
			conversationKey,
			message.ID,
			err,
		)
	}
}

func (d *Dispatcher) appendToPendingLocked(conversationKey string, messages []InboundMessage) bool {
	state := d.pending[conversationKey]
	if state == nil {
		return false
	}
	if !state.active && !state.queued {
		return false
	}

	state.batch = append(state.batch, messages...)
	return true
}

func (d *Dispatcher) markQueuedLocked(conversationKey string) {
	state := d.pending[conversationKey]
	if state == nil {
		state = &pendingConversation{}
		d.pending[conversationKey] = state
	}
	state.queued = true
}

//nolint:contextcheck // This helper intentionally detaches cancellation when re-queueing buffered follow-up work.
func (d *Dispatcher) releaseQueuedConversation(ctx context.Context, conversationKey string) {
	var next InboundMessage
	shouldEnqueue := false

	d.pendingMu.Lock()
	state := d.pending[conversationKey]
	if state != nil {
		state.queued = false
		if !state.active && len(state.batch) > 0 {
			next = combineInboundMessages(state.batch)
			state.batch = nil
			state.queued = true
			shouldEnqueue = true
		}
		if !state.active && !state.queued && len(state.batch) == 0 {
			delete(d.pending, conversationKey)
		}
	}
	d.pendingMu.Unlock()

	if !shouldEnqueue {
		return
	}

	if ctx == nil {
		ctx = context.TODO()
	}
	requeueCtx := context.WithoutCancel(ctx)

	err := d.enqueueReadyMessage(requeueCtx, next)
	if err != nil && !errors.Is(err, ErrDispatcherClosed) {
		log.Printf(
			"dispatcher failed to requeue pending conversation: conversation=%s event_id=%s err=%v",
			conversationKey,
			next.ID,
			err,
		)
	}
}

func (d *Dispatcher) activateConversation(conversationKey string) {
	d.pendingMu.Lock()
	defer d.pendingMu.Unlock()

	state := d.pending[conversationKey]
	if state == nil {
		state = &pendingConversation{}
		d.pending[conversationKey] = state
	}
	state.queued = false
	state.active = true
}

func (d *Dispatcher) nextPendingMessage(conversationKey string) (InboundMessage, bool) {
	d.pendingMu.Lock()
	defer d.pendingMu.Unlock()

	state := d.pending[conversationKey]
	if state == nil {
		return InboundMessage{}, false
	}
	if len(state.batch) == 0 {
		state.active = false
		if !state.queued {
			delete(d.pending, conversationKey)
		}
		return InboundMessage{}, false
	}

	next := combineInboundMessages(state.batch)
	state.batch = nil
	state.active = true
	return next, true
}

func (d *Dispatcher) filterProcessedMessages(ctx context.Context, messages []InboundMessage) ([]InboundMessage, error) {
	freshMessages := make([]InboundMessage, 0, len(messages))
	for _, current := range messages {
		isNew, err := d.store.MarkProcessed(ctx, current.ID)
		if err != nil {
			return nil, err
		}
		if !isNew {
			log.Printf("dispatcher skipped duplicate event: conversation=%s event_id=%s", current.ConversationKey, current.ID)
			continue
		}
		freshMessages = append(freshMessages, current)
	}
	return freshMessages, nil
}

func flattenInboundMessages(message InboundMessage) []InboundMessage {
	if len(message.mergedMessages) == 0 {
		message.mergedMessages = nil
		message.historicalMessages = nil
		return []InboundMessage{message}
	}

	flattened := make([]InboundMessage, 0, len(message.mergedMessages))
	for _, current := range message.mergedMessages {
		flattened = append(flattened, flattenInboundMessages(current)...)
	}
	return flattened
}

func messagesHaveTag(messages []InboundMessage, tag string) bool {
	for _, message := range messages {
		if slices.Contains(message.MessageTags, tag) {
			return true
		}
	}
	return false
}

func isGroupConversationKey(conversationKey string) bool {
	return strings.HasPrefix(conversationKey, "group:") || strings.HasPrefix(conversationKey, "seatalk:group:")
}

func conversationStateInactive(state ConversationState, now time.Time) bool {
	return !state.LastActivityAt.IsZero() && now.Sub(state.LastActivityAt) >= groupConversationMentionTimeout
}

func combineInboundMessages(messages []InboundMessage) InboundMessage {
	switch len(messages) {
	case 0:
		return InboundMessage{}
	case 1:
		single := messages[0]
		single.mergedMessages = nil
		single.historicalMessages = nil
		return single
	default:
		merged := make([]InboundMessage, 0, len(messages))
		for _, current := range messages {
			current.mergedMessages = nil
			current.historicalMessages = nil
			merged = append(merged, current)
		}
		combined := merged[len(merged)-1]
		combined.mergedMessages = merged
		combined.historicalMessages = nil
		return combined
	}
}

func prependHistoricalMessages(message InboundMessage, history []InboundMessage) InboundMessage {
	if len(history) == 0 {
		return message
	}

	current := flattenInboundMessages(message)
	historical := make([]InboundMessage, 0, len(history))
	for _, item := range history {
		item.mergedMessages = nil
		item.historicalMessages = nil
		historical = append(historical, item)
	}

	combined := combineInboundMessages(current)
	combined.historicalMessages = historical
	combined.initialContext = message.initialContext
	combined.LoadInitialContext = nil
	combined.LoadInitialMessages = nil
	return combined
}

func cleanupInboundMessages(ctx context.Context, messages []InboundMessage) error {
	var firstErr error
	for _, message := range messages {
		if message.Responder == nil {
			continue
		}
		err := message.Responder.Cleanup(ctx)
		if err == nil {
			continue
		}
		if firstErr == nil {
			firstErr = err
			continue
		}
		log.Printf("cleanup responder failed: conversation=%s event_id=%s err=%v", message.ConversationKey, message.ID, err)
	}
	return firstErr
}

func (d *Dispatcher) reportFatalError(err error) {
	if err == nil {
		return
	}

	if d.fatalErrCh != nil {
		select {
		case d.fatalErrCh <- err:
		default:
			log.Printf("dispatcher fatal error dropped: %v", err)
		}
	}
}

func (d *Dispatcher) enqueueReadyMessage(ctx context.Context, message InboundMessage) error {
	if d.isClosed() {
		d.releaseQueuedConversation(ctx, message.ConversationKey)
		return ErrDispatcherClosed
	}

	queueLen, err := d.queue.Enqueue(ctx, d.stopCh, message)
	if err != nil {
		d.releaseQueuedConversation(ctx, message.ConversationKey)
		if errors.Is(err, ErrDispatcherClosed) {
			return ErrDispatcherClosed
		}
		return fmt.Errorf("enqueue dispatcher message failed: %w", err)
	}
	log.Printf("dispatcher enqueued message: conversation=%s event_id=%s queue_len=%d", message.ConversationKey, message.ID, queueLen)
	return nil
}

func (d *Dispatcher) initiateShutdown() {
	d.stopOnce.Do(func() {
		d.closeMu.Lock()
		d.closed = true
		d.closeMu.Unlock()
		close(d.stopCh)
	})
}

func (d *Dispatcher) isClosed() bool {
	d.closeMu.RLock()
	defer d.closeMu.RUnlock()
	return d.closed
}

func (d *Dispatcher) dropQueuedWork(ctx context.Context) (int, error) {
	d.pendingMu.Lock()
	droppedMessages := make([]InboundMessage, 0, len(d.delayed))
	for _, state := range d.pending {
		droppedMessages = append(droppedMessages, state.batch...)
	}
	for _, state := range d.delayed {
		droppedMessages = append(droppedMessages, state.batch...)
	}
	d.pending = make(map[string]*pendingConversation)
	d.delayed = make(map[string]*delayedConversation)
	d.pendingMu.Unlock()

	for _, message := range d.queue.Drain() {
		droppedMessages = append(droppedMessages, flattenInboundMessages(message)...)
	}
	err := cleanupInboundMessages(ctx, droppedMessages)
	return len(droppedMessages), err
}

func (d *Dispatcher) dropQueuedCommands(ctx context.Context) (int, error) {
	d.commandMu.Lock()
	dropped := make([]CommandRequest, 0)
	for conversationKey, state := range d.commanding {
		if state == nil || len(state.queue) == 0 {
			continue
		}
		dropped = append(dropped, state.queue...)
		state.queue = nil
		state.blockingCount = 0
		if !state.running {
			delete(d.commanding, conversationKey)
		}
	}
	d.commandMu.Unlock()

	var err error
	for _, req := range dropped {
		cleanupErr := d.cleanupDroppedCommand(ctx, req)
		err = errors.Join(err, cleanupErr)
	}
	return len(dropped), err
}

func (d *Dispatcher) startActiveConversationWork(conversation ConversationState) (*activeConversationWork, func()) {
	d.activeWorkMu.Lock()
	conversationKey := conversation.Key
	active := d.activeWorkByConversation[conversationKey]
	if active == nil {
		d.activeWorkSeq++
		active = &activeConversationWork{
			id:       d.activeWorkSeq,
			turnDone: make(chan struct{}),
			done:     make(chan struct{}),
		}
		if conversationKey != "" {
			d.activeWorkByConversation[conversationKey] = active
		}
	}
	turnID := active.id
	active.conversation = conversation
	d.activeWorkMu.Unlock()

	return active, func() {
		d.activeWorkMu.Lock()
		if conversationKey != "" {
			existing := d.activeWorkByConversation[conversationKey]
			if existing != nil && existing.id == turnID {
				delete(d.activeWorkByConversation, conversationKey)
			}
		}
		d.activeWorkMu.Unlock()
		d.finishActiveConversationWork(active, nil, nil)
		close(active.done)
	}
}

func (d *Dispatcher) claimConversationWork(message InboundMessage) {
	conversationKey := strings.TrimSpace(message.ConversationKey)
	if conversationKey == "" {
		return
	}

	d.activeWorkMu.Lock()
	if d.activeWorkByConversation[conversationKey] == nil {
		d.activeWorkSeq++
		d.activeWorkByConversation[conversationKey] = &activeConversationWork{
			id:       d.activeWorkSeq,
			turnDone: make(chan struct{}),
			done:     make(chan struct{}),
		}
	}
	d.activeWorkMu.Unlock()

	d.activateConversation(conversationKey)
}

func (d *Dispatcher) releaseClaimedConversationWork(conversationKey string) func() {
	conversationKey = strings.TrimSpace(conversationKey)
	if conversationKey == "" {
		return func() {}
	}

	d.activeWorkMu.Lock()
	active := d.activeWorkByConversation[conversationKey]
	if active == nil {
		d.activeWorkMu.Unlock()
		return func() {}
	}
	turnID := active.id
	d.activeWorkMu.Unlock()

	return func() {
		d.activeWorkMu.Lock()
		existing := d.activeWorkByConversation[conversationKey]
		if existing != nil && existing.id == turnID {
			delete(d.activeWorkByConversation, conversationKey)
		} else {
			existing = nil
		}
		d.activeWorkMu.Unlock()
		if existing == nil {
			return
		}
		d.finishActiveConversationWork(existing, nil, nil)
		close(existing.done)
	}
}

func (d *Dispatcher) finishActiveConversationWork(active *activeConversationWork, session Session, turn Turn) {
	if d == nil || active == nil {
		return
	}

	active.mu.Lock()
	defer active.mu.Unlock()

	if session != nil {
		active.session = session
	}
	if turn != nil {
		active.turn = turn
	}
	active.sessionOnce.Do(func() {
		close(active.turnDone)
	})
}

func (d *Dispatcher) acquireConversationSession(ctx context.Context, state ConversationState) (Session, func(), error) {
	if d == nil {
		return nil, func() {}, errors.New("dispatcher is nil")
	}

	conversationKey := state.Key
	resumeSessionID := strings.TrimSpace(state.RunnerThreadID)

	d.sessionsMu.Lock()
	existing := d.sessions[conversationKey]
	var stale Session
	if existing != nil && existing.session != nil {
		sessionID := strings.TrimSpace(existing.session.ID())
		if resumeSessionID == "" || sessionID == "" || sessionID == resumeSessionID {
			session := existing.session
			existing.inUse++
			existing.idleGeneration++
			if existing.idleTimer != nil {
				existing.idleTimer.Stop()
				existing.idleTimer = nil
			}
			d.sessionsMu.Unlock()
			return session, d.releaseConversationSessionFunc(conversationKey, session), nil
		}
		stale = existing.session
		delete(d.sessions, conversationKey)
	}
	d.sessionsMu.Unlock()
	if stale != nil {
		_ = stale.Close()
	}

	session, err := d.runner.StartSession(ctx, SessionOptions{
		ConversationKey: conversationKey,
		ResumeSessionID: resumeSessionID,
	})
	if err != nil {
		return nil, func() {}, err
	}

	d.sessionsMu.Lock()
	if d.sessions == nil {
		d.sessions = make(map[string]*managedConversationSession)
	}
	if existing = d.sessions[conversationKey]; existing != nil && existing.session != nil {
		existing.inUse++
		existing.idleGeneration++
		if existing.idleTimer != nil {
			existing.idleTimer.Stop()
			existing.idleTimer = nil
		}
		d.sessionsMu.Unlock()
		_ = session.Close()
		return existing.session, d.releaseConversationSessionFunc(conversationKey, existing.session), nil
	}
	d.sessions[conversationKey] = &managedConversationSession{
		session:        session,
		inUse:          1,
		idleGeneration: 1,
	}
	d.sessionsMu.Unlock()

	return session, d.releaseConversationSessionFunc(conversationKey, session), nil
}

func (d *Dispatcher) ensureConversationSession(ctx context.Context, state ConversationState) (Session, error) {
	session, releaseSession, err := d.acquireConversationSession(ctx, state)
	if err != nil {
		return nil, err
	}
	releaseSession()
	return session, nil
}

func (d *Dispatcher) releaseConversationSessionFunc(conversationKey string, session Session) func() {
	return func() {
		d.endConversationSessionUse(conversationKey, session)
	}
}

func (d *Dispatcher) beginConversationSessionUse(conversationKey string, session Session) {
	if d == nil || strings.TrimSpace(conversationKey) == "" || session == nil {
		return
	}

	d.sessionsMu.Lock()
	managed := d.sessions[conversationKey]
	if managed == nil || managed.session != session {
		d.sessionsMu.Unlock()
		return
	}
	managed.inUse++
	managed.idleGeneration++
	if managed.idleTimer != nil {
		managed.idleTimer.Stop()
		managed.idleTimer = nil
	}
	d.sessionsMu.Unlock()
}

func (d *Dispatcher) endConversationSessionUse(conversationKey string, session Session) {
	if d == nil || strings.TrimSpace(conversationKey) == "" || session == nil {
		return
	}
	timeout := d.sessionIdleTimeout
	if timeout <= 0 {
		return
	}

	d.sessionsMu.Lock()
	managed := d.sessions[conversationKey]
	if managed == nil || managed.session != session {
		d.sessionsMu.Unlock()
		return
	}
	if managed.inUse > 0 {
		managed.inUse--
	}
	if managed.inUse > 0 {
		d.sessionsMu.Unlock()
		return
	}
	managed.idleGeneration++
	generation := managed.idleGeneration
	if managed.idleTimer != nil {
		managed.idleTimer.Stop()
	}
	managed.idleTimer = time.AfterFunc(timeout, func() {
		d.expireIdleConversationSession(conversationKey, session, generation)
	})
	d.sessionsMu.Unlock()
}

func (d *Dispatcher) expireIdleConversationSession(conversationKey string, session Session, generation uint64) {
	if d == nil || strings.TrimSpace(conversationKey) == "" || session == nil {
		return
	}

	d.sessionsMu.Lock()
	managed := d.sessions[conversationKey]
	if managed == nil || managed.session != session || managed.inUse != 0 || managed.idleGeneration != generation {
		d.sessionsMu.Unlock()
		return
	}
	if managed.idleTimer != nil {
		managed.idleTimer.Stop()
		managed.idleTimer = nil
	}
	delete(d.sessions, conversationKey)
	d.sessionsMu.Unlock()

	log.Printf("dispatcher expiring idle session: conversation=%s session_id=%s idle_timeout=%s", conversationKey, session.ID(), d.sessionIdleTimeout)
	if err := session.Close(); err != nil {
		log.Printf("dispatcher idle session close failed: conversation=%s session_id=%s err=%v", conversationKey, session.ID(), err)
	}
}

func (d *Dispatcher) managedConversationSession(conversationKey string) Session {
	d.sessionsMu.Lock()
	defer d.sessionsMu.Unlock()

	managed := d.sessions[conversationKey]
	if managed == nil {
		return nil
	}
	return managed.session
}

func (d *Dispatcher) dropConversationSession(conversationKey string) Session {
	d.sessionsMu.Lock()
	defer d.sessionsMu.Unlock()

	managed := d.sessions[conversationKey]
	if managed == nil {
		return nil
	}
	if managed.idleTimer != nil {
		managed.idleTimer.Stop()
		managed.idleTimer = nil
	}
	delete(d.sessions, conversationKey)
	return managed.session
}

func (d *Dispatcher) discardConversationSession(conversationKey string, session Session) {
	if d == nil || strings.TrimSpace(conversationKey) == "" || session == nil {
		return
	}

	dropped := func() Session {
		d.sessionsMu.Lock()
		defer d.sessionsMu.Unlock()

		managed := d.sessions[conversationKey]
		if managed == nil || managed.session != session {
			return nil
		}
		if managed.idleTimer != nil {
			managed.idleTimer.Stop()
			managed.idleTimer = nil
		}
		delete(d.sessions, conversationKey)
		return managed.session
	}()
	if dropped == nil {
		return
	}
	if err := dropped.Close(); err != nil {
		log.Printf("dispatcher session discard close failed: conversation=%s session_id=%s err=%v", conversationKey, dropped.ID(), err)
	}
}

func (d *Dispatcher) persistConversationSessionID(ctx context.Context, state ConversationState, session Session) {
	if d == nil || session == nil || strings.TrimSpace(state.Key) == "" {
		return
	}
	if strings.TrimSpace(state.RunnerThreadID) != "" {
		return
	}
	sessionID := strings.TrimSpace(session.ID())
	if sessionID == "" {
		return
	}

	state.RunnerThreadID = sessionID
	if state.LastActivityAt.IsZero() {
		state.LastActivityAt = time.Now()
	}
	if err := d.store.PutConversation(context.WithoutCancel(ctx), state); err != nil {
		log.Printf("dispatcher failed to persist session id: conversation=%s session_id=%s err=%v", state.Key, sessionID, err)
		return
	}
	log.Printf("dispatcher persisted session id before turn execution: conversation=%s session_id=%s", state.Key, sessionID)
}

func (d *Dispatcher) closeSessionsAfterWorkers() {
	go func() {
		d.workersWG.Wait()
		d.commandWG.Wait()
		if err := d.closeSessionsOnce(); err != nil {
			log.Printf("dispatcher deferred session close failed: err=%v", err)
		}
	}()
}

func (d *Dispatcher) closeSessionsOnce() error {
	var err error
	d.sessionsCloseOnce.Do(func() {
		err = d.closeSessions()
	})
	return err
}

func (d *Dispatcher) closeSessions() error {
	d.sessionsMu.Lock()
	sessions := make([]Session, 0, len(d.sessions))
	for key, managed := range d.sessions {
		if managed == nil || managed.session == nil {
			delete(d.sessions, key)
			continue
		}
		if managed.idleTimer != nil {
			managed.idleTimer.Stop()
			managed.idleTimer = nil
		}
		sessions = append(sessions, managed.session)
		delete(d.sessions, key)
	}
	d.sessionsMu.Unlock()

	if len(sessions) == 0 {
		return nil
	}

	closeErrs := make(chan error, len(sessions))
	var wg sync.WaitGroup
	for _, session := range sessions {
		if session == nil {
			continue
		}
		wg.Add(1)
		go func(current Session) {
			defer wg.Done()
			closeErrs <- current.Close()
		}(session)
	}
	wg.Wait()
	close(closeErrs)

	var closeErr error
	for err := range closeErrs {
		closeErr = errors.Join(closeErr, err)
	}
	return closeErr
}

// EnqueueCommand queues one slash command outside the normal turn queue.
func (d *Dispatcher) EnqueueCommand(_ context.Context, req CommandRequest) error {
	if d == nil {
		return errors.New("enqueue command failed: dispatcher is nil")
	}
	if req.ConversationKey == "" {
		return errors.New("enqueue command failed: conversation key is empty")
	}
	if !req.Command.Validate() {
		return errors.New("enqueue command failed: invalid slash command")
	}
	if d.isClosed() {
		return ErrDispatcherClosed
	}
	spec, ok := d.resolveCommandSpec(req.Command)
	if !ok {
		return errors.New("enqueue command failed: unsupported slash command")
	}
	req.resolvedSpec = spec

	shouldStart := false
	d.commandMu.Lock()
	state := d.commanding[req.ConversationKey]
	if state == nil {
		state = &commandConversation{}
		d.commanding[req.ConversationKey] = state
		shouldStart = true
	}
	if req.resolvedSpec.Interrupts {
		state.blockingCount++
	}
	state.queue = append(state.queue, req)
	d.commandMu.Unlock()

	if !shouldStart {
		return nil
	}

	d.commandWG.Add(1)
	//nolint:contextcheck // Command processing is intentionally detached from the callback request context.
	go d.runCommandQueue(req.ConversationKey)
	return nil
}

func (d *Dispatcher) runCommandQueue(conversationKey string) {
	defer d.commandWG.Done()

	for {
		req, ok := d.nextCommand(conversationKey)
		if !ok {
			return
		}
		d.executeCommand(req)
	}
}

func (d *Dispatcher) nextCommand(conversationKey string) (CommandRequest, bool) {
	d.commandMu.Lock()
	defer d.commandMu.Unlock()

	state := d.commanding[conversationKey]
	if state == nil {
		return CommandRequest{}, false
	}
	if len(state.queue) == 0 {
		state.running = false
		if state.blockingCount == 0 {
			delete(d.commanding, conversationKey)
		}
		return CommandRequest{}, false
	}

	req := state.queue[0]
	state.queue = state.queue[1:]
	state.running = true
	return req, true
}

func (d *Dispatcher) executeCommand(req CommandRequest) {
	commandCtx := context.Background()
	defer d.cleanupCommandResponder(commandCtx, req)

	var (
		replyText string
		err       error
	)
	if req.resolvedSpec.Interrupts {
		replyText, err = d.executeBlockingCommand(commandCtx, req)
	} else {
		replyText, err = d.buildCommandReply(commandCtx, req)
	}
	if err != nil {
		d.finishCommand(req)
		log.Printf("dispatcher command failed: conversation=%s command=%s err=%v", req.ConversationKey, req.Command.Name, err)
		return
	}
	d.sendCommandReply(commandCtx, req, replyText)
	d.finishCommand(req)
}

func (d *Dispatcher) finishCommand(req CommandRequest) {
	d.commandMu.Lock()
	defer d.commandMu.Unlock()

	state := d.commanding[req.ConversationKey]
	if state == nil {
		return
	}
	state.running = false
	if req.resolvedSpec.Interrupts && state.blockingCount > 0 {
		state.blockingCount--
	}
	if len(state.queue) == 0 && state.blockingCount == 0 {
		delete(d.commanding, req.ConversationKey)
	}
}

func (d *Dispatcher) executeBlockingCommand(ctx context.Context, req CommandRequest) (string, error) {
	interrupted, err := d.interruptCommandConversation(ctx, req)
	if err != nil {
		return "", err
	}

	if req.Command.IsStop() {
		muted := strings.EqualFold(strings.TrimSpace(req.Command.Args), slashCommandStopMute)
		if muted {
			unlock := d.locks.Lock(req.ConversationKey)
			defer unlock()
		}
		return d.buildStopCommandReply(ctx, req, interrupted, muted)
	}

	unlock := d.locks.Lock(req.ConversationKey)
	defer unlock()

	return d.buildBlockingCommandReplyLocked(ctx, req, interrupted)
}

func (d *Dispatcher) buildStopCommandReply(ctx context.Context, req CommandRequest, interrupted bool, muted bool) (string, error) {
	clearedMessages, cleanupErr := d.dropConversationQueuedWork(ctx, req.ConversationKey)
	if cleanupErr != nil {
		return "", cleanupErr
	}

	for _, message := range clearedMessages {
		d.recordCommandMessage(req.ConversationKey, message)
	}

	if muted {
		state, _, err := d.loadCommandConversationState(ctx, req)
		if err != nil {
			return "", err
		}
		state.Muted = true
		state.LastEventID = req.EventID
		state.LastActivityAt = time.Now()
		if err = d.store.PutConversation(context.WithoutCancel(ctx), state); err != nil {
			return "", fmt.Errorf("persist muted conversation state failed: %w", err)
		}
		log.Printf("dispatcher muted conversation: conversation=%s event_id=%s", req.ConversationKey, req.EventID)
	}

	return buildStopReply(interrupted, muted, d.takeCommandBriefs(req.ConversationKey)), nil
}

func (d *Dispatcher) buildCommandReply(ctx context.Context, req CommandRequest) (string, error) {
	switch {
	case req.Command.IsHelp():
		return d.buildHelpReply(), nil
	case req.Command.IsStatus():
		return d.buildStatusReply(ctx, req)
	default:
		return "", errors.New("unsupported slash command")
	}
}

func (d *Dispatcher) buildBlockingCommandReplyLocked(ctx context.Context, req CommandRequest, interrupted bool) (string, error) {
	switch {
	case req.Command.IsReset():
		return d.buildResetCommandReplyLocked(ctx, req, interrupted)
	default:
		return "", errors.New("unsupported slash command")
	}
}

func (d *Dispatcher) interruptCommandConversation(ctx context.Context, req CommandRequest) (bool, error) {
	interrupted := false
	if active := d.activeConversationWork(req.ConversationKey); active != nil {
		interrupted = true
		active.interrupted.Store(true)
		active.mu.Lock()
		turn := active.turn
		turnDone := active.turnDone
		active.mu.Unlock()
		if turn == nil && turnDone != nil {
			select {
			case <-turnDone:
			case <-active.done:
			}
			active.mu.Lock()
			turn = active.turn
			active.mu.Unlock()
		}
		if turn != nil {
			interruptCtx, interruptCancel := context.WithTimeout(ctx, 5*time.Second)
			defer interruptCancel()
			if interruptErr := turn.Interrupt(interruptCtx); interruptErr != nil {
				log.Printf("dispatcher turn interrupt failed: conversation=%s session_id=%s err=%v", req.ConversationKey, active.session.ID(), interruptErr)
			}
			d.persistConversationAfterInterrupt(ctx, req.ConversationKey, active.session)
			<-active.done
			return interrupted, nil
		}
		<-active.done
	}

	return interrupted, nil
}

func (d *Dispatcher) persistConversationAfterInterrupt(ctx context.Context, conversationKey string, session Session) {
	if d == nil || strings.TrimSpace(conversationKey) == "" || session == nil {
		return
	}

	state, err := d.store.GetConversation(ctx, conversationKey)
	if err != nil {
		if !errors.Is(err, cache.ErrNotFound) {
			log.Printf("dispatcher failed to load conversation for interrupt persistence: conversation=%s err=%v", conversationKey, err)
		}
		state = ConversationState{Key: conversationKey}
	}
	if sessionID := strings.TrimSpace(session.ID()); sessionID != "" {
		state.RunnerThreadID = sessionID
	}
	if err = d.store.PutConversation(context.WithoutCancel(ctx), state); err != nil {
		log.Printf("dispatcher failed to persist conversation after interrupt: conversation=%s session_id=%s err=%v", conversationKey, session.ID(), err)
	}
}

func (d *Dispatcher) buildResetCommandReplyLocked(ctx context.Context, req CommandRequest, interrupted bool) (string, error) {
	clearedMessages, cleanupErr := d.dropConversationQueuedWork(ctx, req.ConversationKey)
	if cleanupErr != nil {
		return "", cleanupErr
	}

	for _, message := range clearedMessages {
		d.recordCommandMessage(req.ConversationKey, message)
	}

	if dropped := d.dropConversationSession(req.ConversationKey); dropped != nil {
		if closeErr := dropped.Close(); closeErr != nil {
			log.Printf("dispatcher session close failed during reset: conversation=%s session_id=%s err=%v", req.ConversationKey, dropped.ID(), closeErr)
		}
	}
	if deleteErr := d.store.DeleteConversation(context.WithoutCancel(ctx), req.ConversationKey); deleteErr != nil {
		return "", fmt.Errorf("delete conversation state failed: %w", deleteErr)
	}

	return buildResetReply(interrupted, d.takeCommandBriefs(req.ConversationKey)), nil
}

func (d *Dispatcher) buildStatusReply(ctx context.Context, req CommandRequest) (string, error) {
	var err error
	session, releaseSession, starting := d.peekStatusMemorySession(req.ConversationKey)
	switch {
	case starting:
		return "_Current conversation status:_ _starting_", nil
	case session != nil:
	default:
		session, releaseSession, starting, err = d.tryLoadStatusSession(ctx, req)
		if err != nil {
			return "", err
		}
		if starting {
			return "_Current conversation status:_ _starting_", nil
		}
	}
	if session != nil {
		defer releaseSession()
		var status SessionStatus
		status, err = session.Status(ctx)
		if err != nil {
			return "", err
		}
		return renderStatusReply(status), nil
	}
	return "_Current conversation status:_ _inactive_", nil
}

func (d *Dispatcher) tryLoadStatusSession(ctx context.Context, req CommandRequest) (Session, func(), bool, error) {
	if d == nil {
		return nil, func() {}, false, nil
	}

	conversationKey := req.ConversationKey
	unlock := d.locks.TryLock(conversationKey)
	if unlock == nil {
		return nil, func() {}, false, nil
	}
	defer unlock()

	session, releaseSession, starting := d.peekStatusMemorySession(conversationKey)
	if starting || session != nil {
		return session, releaseSession, starting, nil
	}

	state, stateExists, err := d.loadCommandConversationState(ctx, req)
	if err != nil {
		return nil, func() {}, false, err
	}
	if !stateExists || strings.TrimSpace(state.RunnerThreadID) == "" {
		return nil, func() {}, false, nil
	}

	session, releaseSession, err = d.acquireConversationSession(ctx, state)
	if err != nil {
		return nil, func() {}, false, err
	}
	return session, releaseSession, false, nil
}

func (d *Dispatcher) loadCommandConversationState(ctx context.Context, req CommandRequest) (ConversationState, bool, error) {
	state, err := d.store.GetConversation(ctx, req.ConversationKey)
	if err == nil {
		return state, true, nil
	}
	if !errors.Is(err, cache.ErrNotFound) {
		return ConversationState{}, false, err
	}
	return ConversationState{
		Key:            req.ConversationKey,
		LastEventID:    req.EventID,
		LastActivityAt: time.Now(),
	}, false, nil
}

func (d *Dispatcher) peekStatusMemorySession(conversationKey string) (Session, func(), bool) {
	if managed := d.managedConversationSession(conversationKey); managed != nil {
		d.beginConversationSessionUse(conversationKey, managed)
		return managed, d.releaseConversationSessionFunc(conversationKey, managed), false
	}

	if session, active, starting := d.activeWorkStatusSession(conversationKey); active {
		return session, func() {}, starting
	}

	return nil, func() {}, false
}

func (d *Dispatcher) sendCommandReply(ctx context.Context, req CommandRequest, replyText string) {
	if req.Responder == nil {
		if err := (LoggingResponder{}).SendText(ctx, replyText); err != nil {
			log.Printf("dispatcher command reply failed: conversation=%s err=%v", req.ConversationKey, err)
		}
		return
	}
	if err := req.Responder.SendText(ctx, replyText); err != nil {
		log.Printf("dispatcher command reply failed: conversation=%s err=%v", req.ConversationKey, err)
	}
}

func (d *Dispatcher) tryRecordCommandDrop(message InboundMessage) bool {
	flattened := flattenInboundMessages(message)
	if len(flattened) == 0 {
		return false
	}

	d.commandMu.Lock()
	defer d.commandMu.Unlock()

	state := d.commanding[message.ConversationKey]
	if state == nil || state.blockingCount == 0 {
		return false
	}
	for _, current := range flattened {
		brief := messageBrief(current)
		if brief == "" {
			continue
		}
		state.briefs = append(state.briefs, brief)
	}
	return true
}

func (d *Dispatcher) recordCommandMessage(conversationKey string, message InboundMessage) {
	brief := messageBrief(message)
	if brief == "" {
		return
	}

	d.commandMu.Lock()
	defer d.commandMu.Unlock()
	state := d.commanding[conversationKey]
	if state == nil {
		return
	}
	state.briefs = append(state.briefs, brief)
}

func (d *Dispatcher) takeCommandBriefs(conversationKey string) []string {
	d.commandMu.Lock()
	defer d.commandMu.Unlock()
	state := d.commanding[conversationKey]
	if state == nil || len(state.briefs) == 0 {
		return nil
	}
	briefs := append([]string(nil), state.briefs...)
	state.briefs = nil
	return briefs
}

func (d *Dispatcher) activeWorkStatusSession(conversationKey string) (Session, bool, bool) {
	active := d.activeConversationWork(conversationKey)
	if active == nil {
		return nil, false, false
	}

	select {
	case <-active.turnDone:
		return active.session, true, false
	default:
		return nil, true, true
	}
}

func (d *Dispatcher) activeConversationWork(conversationKey string) *activeConversationWork {
	d.activeWorkMu.Lock()
	defer d.activeWorkMu.Unlock()

	return d.activeWorkByConversation[conversationKey]
}

func (d *Dispatcher) cleanupCommandResponder(ctx context.Context, req CommandRequest) {
	if req.Responder == nil {
		return
	}
	if err := req.Responder.Cleanup(ctx); err != nil {
		log.Printf("cleanup responder failed after command: conversation=%s event_id=%s err=%v", req.ConversationKey, req.EventID, err)
	}
}

func (d *Dispatcher) cleanupDroppedCommand(ctx context.Context, req CommandRequest) error {
	if req.Responder == nil {
		return nil
	}
	if err := req.Responder.Cleanup(ctx); err != nil {
		log.Printf(
			"cleanup responder failed after dropped queued command: conversation=%s event_id=%s command=%s err=%v",
			req.ConversationKey,
			req.EventID,
			req.Command.Name,
			err,
		)
		return err
	}
	return nil
}

func (d *Dispatcher) dropConversationQueuedWork(ctx context.Context, conversationKey string) ([]InboundMessage, error) {
	d.pendingMu.Lock()
	dropped := make([]InboundMessage, 0)
	if state := d.pending[conversationKey]; state != nil {
		dropped = append(dropped, state.batch...)
		delete(d.pending, conversationKey)
	}
	if state := d.delayed[conversationKey]; state != nil {
		dropped = append(dropped, state.batch...)
		delete(d.delayed, conversationKey)
	}
	d.pendingMu.Unlock()

	removed := d.queue.RemoveMatching(func(message InboundMessage) bool {
		return message.ConversationKey == conversationKey
	})
	for _, message := range removed {
		dropped = append(dropped, flattenInboundMessages(message)...)
	}
	return dropped, cleanupInboundMessages(ctx, dropped)
}

func buildStopReply(interrupted bool, muted bool, briefs []string) string {
	lines := make([]string, 0, len(briefs)+4)
	switch {
	case interrupted && muted:
		lines = append(lines, "_Conversation interrupted and muted._")
	case muted:
		lines = append(lines, "_Conversation muted._")
	case interrupted:
		lines = append(lines, "_Conversation interrupted._")
	default:
		lines = append(lines, "_Conversation is not running._")
	}
	if muted {
		lines = append(lines, "_Future messages in this conversation will be ignored._")
	} else {
		lines = append(lines, "_Quote the messages above if you want to continue from them._")
	}
	if len(briefs) == 0 {
		return strings.Join(lines, "\n")
	}
	lines = append(lines, "_Discarded unprocessed messages:_")
	for _, brief := range briefs {
		lines = append(lines, "- _"+brief+"_")
	}
	return strings.Join(lines, "\n")
}

func buildResetReply(interrupted bool, briefs []string) string {
	lines := make([]string, 0, len(briefs)+4)
	if interrupted {
		lines = append(lines, "_Conversation interrupted and reset._")
	} else {
		lines = append(lines, "_Conversation reset._")
	}
	lines = append(lines, "_A follow-up message will start a new agent thread and reload initial history._")
	if len(briefs) == 0 {
		return strings.Join(lines, "\n")
	}
	lines = append(lines, "_Discarded unprocessed messages:_")
	for _, brief := range briefs {
		lines = append(lines, "- _"+brief+"_")
	}
	return strings.Join(lines, "\n")
}

func (d *Dispatcher) resolveCommandSpec(command SlashCommand) (CommandSpec, bool) {
	return DispatcherCommandSpec(command.Name)
}

func (d *Dispatcher) buildHelpReply() string {
	specs := SupportedDispatcherSlashCommands()

	lines := make([]string, 0, len(specs)+2)
	lines = append(lines, "_Supported slash commands:_")
	for _, spec := range specs {
		lines = append(lines, fmt.Sprintf("- `%s`: _%s_", spec.Usage, spec.Description))
	}
	return strings.Join(lines, "\n")
}

func renderStatusReply(status SessionStatus) string {
	lines := make([]string, 0, 4+len(status.ConfigOptions)*2)
	lines = append(lines, "_Current conversation status:_")
	lines = append(lines, "- _Agent_: `"+formatStatusValue(status.Agent)+"`")
	lines = append(lines, "- _Working directories_: `"+formatStatusDirectories(status.WorkingDirectories)+"`")
	lines = append(lines, "- _Current mode_: `"+formatStatusValue(status.Modes.CurrentModeID)+"`")
	lines = append(lines, formatStatusConfigOptions(status.ConfigOptions)...)
	return strings.Join(lines, "\n")
}

func formatStatusConfigOptions(options []SessionConfigOption) []string {
	if len(options) == 0 {
		return nil
	}

	lines := make([]string, 0, 1+len(options)*3)
	lines = append(lines, "- _Config options:_")
	for _, option := range options {
		name := formatStatusValue(option.Name)
		currentValue := formatStatusValue(option.CurrentValue)
		lines = append(lines, "  - _Name_: `"+name+"`")
		lines = append(lines, "    - _Current value_: `"+currentValue+"`")
	}
	return lines
}

func formatStatusDirectories(directories []string) string {
	if len(directories) == 0 {
		return "n/a"
	}

	seen := make(map[string]struct{}, len(directories))
	filtered := make([]string, 0, len(directories))
	for _, directory := range directories {
		trimmed := strings.TrimSpace(directory)
		if trimmed == "" {
			continue
		}
		if _, ok := seen[trimmed]; ok {
			continue
		}
		seen[trimmed] = struct{}{}
		filtered = append(filtered, trimmed)
	}
	if len(filtered) == 0 {
		return "n/a"
	}
	return strings.Join(filtered, ", ")
}

func formatStatusValue(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" || trimmed == "<nil>" {
		return "n/a"
	}
	return trimmed
}

func messageBrief(message InboundMessage) string {
	text := strings.TrimSpace(message.Text)
	switch {
	case text != "":
	case len(message.ImagePaths) != 0 || message.ImagePath != "":
		text = "[image]"
	case len(message.FilePaths) != 0 || message.FilePath != "":
		text = "[file]"
	case len(message.VideoPaths) != 0 || message.VideoPath != "":
		text = "[video]"
	case message.Kind != "":
		text = "[" + string(message.Kind) + "]"
	}
	text = strings.Join(strings.Fields(text), " ")
	if text == "" {
		return "message"
	}
	const maxBriefLen = 80
	if len(text) <= maxBriefLen {
		return text
	}
	return strings.TrimSpace(text[:maxBriefLen]) + "..."
}

func newConversationState(message InboundMessage) ConversationState {
	return ConversationState{
		Key:            message.ConversationKey,
		LastEventID:    message.ID,
		LastActivityAt: time.Now(),
	}
}

func shouldDelayInboundMessage(message InboundMessage) bool {
	switch message.Kind {
	case MessageKindImage, MessageKindMixed, MessageKindForwarded, MessageKindFile, MessageKindVideo, MessageKindInteractiveCard:
		return true
	default:
		return false
	}
}
