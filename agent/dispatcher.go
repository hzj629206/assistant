package agent

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/hzj629206/assistant/cache"
)

const defaultQueueSize = 128
const defaultWorkerCount = 4
const defaultNonTextMergeWindow = 10 * time.Second
const defaultShutdownTurnTimeout = 15 * time.Second

// ErrDispatcherClosed indicates the dispatcher is no longer accepting new work.
var ErrDispatcherClosed = errors.New("dispatcher is closed")

// DispatcherOptions configures the asynchronous callback dispatcher.
type DispatcherOptions struct {
	Store               *ConversationStore
	Runner              Runner
	FatalErrCh          chan<- error
	QueueSize           int
	WorkerCount         int
	NonTextMergeWindow  time.Duration
	ShutdownTurnTimeout time.Duration
}

// Dispatcher normalizes callback events and runs them asynchronously.
type Dispatcher struct {
	store               *ConversationStore
	runner              Runner
	queue               chan InboundMessage
	workerCount         int
	mergeWindow         time.Duration
	locks               *keyedLocker
	pendingMu           sync.Mutex
	pending             map[string]*pendingConversation
	delayed             map[string]*delayedConversation
	startOnce           sync.Once
	stopOnce            sync.Once
	workersDone         chan struct{}
	workersWG           sync.WaitGroup
	closeMu             sync.RWMutex
	closed              bool
	stopCh              chan struct{}
	fatalErrCh          chan<- error
	shutdownTurnTimeout time.Duration
	activeTurnsMu       sync.Mutex
	activeTurns         map[uint64]context.CancelFunc
	activeTurnSeq       uint64
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

	runner := options.Runner
	if runner == nil {
		runner = &NoopRunner{}
	}

	mergeWindow := options.NonTextMergeWindow
	if mergeWindow <= 0 {
		mergeWindow = defaultNonTextMergeWindow
	}

	return &Dispatcher{
		store:               store,
		runner:              runner,
		queue:               make(chan InboundMessage, queueSize),
		workerCount:         workerCount,
		mergeWindow:         mergeWindow,
		locks:               newKeyedLocker(),
		pending:             make(map[string]*pendingConversation),
		delayed:             make(map[string]*delayedConversation),
		workersDone:         make(chan struct{}),
		stopCh:              make(chan struct{}),
		fatalErrCh:          options.FatalErrCh,
		shutdownTurnTimeout: options.ShutdownTurnTimeout,
		activeTurns:         make(map[uint64]context.CancelFunc),
	}
}

// Start launches background workers.
func (d *Dispatcher) Start() error {
	d.startOnce.Do(func() {
		log.Printf("dispatcher starting workers: worker_count=%d queue_size=%d", d.workerCount, cap(d.queue))
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

// Shutdown stops accepting new work, drops queued-but-not-running work, and waits for running turns.
func (d *Dispatcher) Shutdown(ctx context.Context) error {
	d.initiateShutdown()
	droppedMessages, cleanupErr := d.dropQueuedWork(context.Background()) //nolint:contextcheck
	log.Printf("dispatcher shutdown requested")
	if droppedMessages > 0 {
		log.Printf("dispatcher dropped queued work during shutdown: message_count=%d", droppedMessages)
	}

	waitErr := d.waitForWorkers(ctx)
	if waitErr != nil {
		return errors.Join(cleanupErr, waitErr)
	}

	log.Printf("dispatcher shutdown completed")
	return cleanupErr
}

func (d *Dispatcher) waitForWorkers(ctx context.Context) error {
	shutdownTurnTimeout := d.shutdownTurnTimeout
	if shutdownTurnTimeout <= 0 {
		shutdownTurnTimeout = defaultShutdownTurnTimeout
	}

	timer := time.NewTimer(shutdownTurnTimeout)
	defer timer.Stop()

	select {
	case <-d.workersDone:
		return nil
	case <-timer.C:
		log.Printf("dispatcher shutdown grace period elapsed; canceling running turns")
		d.cancelActiveTurns()
	case <-ctx.Done():
		d.cancelActiveTurns()
		return ctx.Err()
	}

	select {
	case <-d.workersDone:
		return nil
	case <-ctx.Done():
		d.cancelActiveTurns()
		return ctx.Err()
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

		var message InboundMessage
		select {
		case <-d.stopCh:
			return
		case message = <-d.queue:
		}

		current := message
		for {
			d.activateConversation(current.ConversationKey)
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

	unlock := d.locks.Lock(message.ConversationKey)
	defer unlock()

	state, err := d.store.GetConversation(ctx, message.ConversationKey)
	isNewConversation := false
	if err != nil {
		if !errors.Is(err, cache.ErrNotFound) {
			return err
		}
		isNewConversation = true
		state = newConversationState(message)
		log.Printf("dispatcher created conversation state: conversation=%s event_id=%s", message.ConversationKey, message.ID)
	}
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

	log.Printf("dispatcher running turn: conversation=%s event_id=%s existing_thread=%s", message.ConversationKey, message.ID, state.CodexThreadID)
	runCtx, releaseTurn := d.startTurnContext(ctx)
	defer releaseTurn()

	result, err := d.runner.RunTurn(runCtx, TurnRequest{
		Conversation: state,
		Message:      message,
	})
	if err != nil {
		return err
	}
	log.Printf(
		"dispatcher completed turn: conversation=%s event_id=%s thread_id=%s reply_len=%d",
		message.ConversationKey,
		message.ID,
		result.CodexThreadID,
		len(result.ReplyText),
	)

	state.LastEventID = message.ID
	state.LastActivityAt = time.Now()
	if result.CodexThreadID != "" {
		state.CodexThreadID = result.CodexThreadID
	}

	if err := d.store.PutConversation(ctx, state); err != nil {
		return err
	}
	log.Printf("dispatcher stored conversation state: conversation=%s event_id=%s thread_id=%s", state.Key, message.ID, state.CodexThreadID)

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

func (d *Dispatcher) releaseQueuedConversation(conversationKey string) {
	d.pendingMu.Lock()
	defer d.pendingMu.Unlock()

	state := d.pending[conversationKey]
	if state == nil {
		return
	}
	state.queued = false
	if !state.active && len(state.batch) == 0 {
		delete(d.pending, conversationKey)
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
		d.releaseQueuedConversation(message.ConversationKey)
		return ErrDispatcherClosed
	}

	select {
	case <-d.stopCh:
		d.releaseQueuedConversation(message.ConversationKey)
		return ErrDispatcherClosed
	case d.queue <- message:
		queueLen := len(d.queue)
		log.Printf("dispatcher enqueued message: conversation=%s event_id=%s queue_len=%d", message.ConversationKey, message.ID, queueLen)
		return nil
	case <-ctx.Done():
		d.releaseQueuedConversation(message.ConversationKey)
		return fmt.Errorf("enqueue dispatcher message failed: %w", ctx.Err())
	}
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

	for {
		select {
		case message := <-d.queue:
			droppedMessages = append(droppedMessages, flattenInboundMessages(message)...)
		default:
			err := cleanupInboundMessages(ctx, droppedMessages)
			return len(droppedMessages), err
		}
	}
}

func (d *Dispatcher) startTurnContext(parent context.Context) (context.Context, func()) {
	if parent == nil {
		parent = context.Background()
	}

	turnCtx, cancel := context.WithCancel(parent)

	d.activeTurnsMu.Lock()
	d.activeTurnSeq++
	turnID := d.activeTurnSeq
	d.activeTurns[turnID] = cancel
	d.activeTurnsMu.Unlock()

	return turnCtx, func() {
		cancel()
		d.activeTurnsMu.Lock()
		delete(d.activeTurns, turnID)
		d.activeTurnsMu.Unlock()
	}
}

func (d *Dispatcher) cancelActiveTurns() {
	d.activeTurnsMu.Lock()
	cancels := make([]context.CancelFunc, 0, len(d.activeTurns))
	for _, cancel := range d.activeTurns {
		cancels = append(cancels, cancel)
	}
	d.activeTurnsMu.Unlock()

	for _, cancel := range cancels {
		cancel()
	}
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
