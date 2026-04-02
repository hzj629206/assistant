package adapter

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/hzj629206/assistant/agent"
	"github.com/hzj629206/assistant/seatalk"
)

const seatalkTypingStatusWindow = 30 * time.Second

// SeaTalkAgentAdapter bridges the agent core with the SeaTalk platform.
type SeaTalkAgentAdapter struct {
	dispatcher *agent.Dispatcher
	client     *seatalk.Client
	router     seaTalkRouter
}

// NewSeaTalkAgentAdapter builds a SeaTalk-backed agent adapter.
func NewSeaTalkAgentAdapter(dispatcher *agent.Dispatcher, cfg seatalk.Config) *SeaTalkAgentAdapter {
	return newSeaTalkAgentAdapterWithClient(dispatcher, seatalk.NewClient(cfg))
}

func newSeaTalkAgentAdapterWithClient(dispatcher *agent.Dispatcher, client *seatalk.Client) *SeaTalkAgentAdapter {
	return &SeaTalkAgentAdapter{
		dispatcher: dispatcher,
		client:     client,
	}
}

// SystemPrompt returns SeaTalk-specific operating guidance for the model.
func (a *SeaTalkAgentAdapter) SystemPrompt() string {
	if a == nil || a.client == nil {
		return ""
	}

	return strings.TrimSpace(`
When you need to deliver a generated artifact such as a CSV, JSON, text report, or other data file to the user in SeaTalk, use the seatalk_send_file tool.
When you need the user to choose between explicit actions, confirm a risky operation, or provide approval in SeaTalk, prefer sending an interactive message instead of a plain text question.
When you need to present complex results in SeaTalk, consider using interactive cards instead of relying only on plain text.
When running complex tasks, consider using interactive cards during task execution to report intermediate status and progress updates.
When you need to mention a user in SeaTalk, use one of these tags:
- <mention-tag target="seatalk://user?id=SEATALK_ID"/>
- <mention-tag target="seatalk://user?email=USER_EMAIL"/>
USER_EMAIL is limited to corporate addresses under @sea.com, @shopee.com, or @monee.com.
For callback buttons, encode the button "value" as a JSON callback action payload.
Supported callback action payloads:
- Tool call: {"action":"tool_call","tool_name":"...","tool_input_json":"{...}"}
- Prompt submission: {"action":"prompt","prompt":"..."}
The system may replace oversized valid callback payloads with a short internal reference automatically before sending them to SeaTalk.
When handling an interactive button click, treat a tool_call action as the user's selected tool call and execute that tool directly. Treat a prompt action as a new user prompt submitted into the current conversation.
`)
}

// Tools returns the SeaTalk-specific tools that can be registered on the agent runner.
func (a *SeaTalkAgentAdapter) Tools() []agent.Tool {
	if a == nil || a.client == nil {
		return nil
	}

	tools := []agent.Tool{
		seaTalkSendFileTool{},
		seaTalkSendInteractiveMessageTool{},
		seaTalkUpdateInteractiveMessageTool{},
		seaTalkGetEmployeeInfoTool{client: a.client},
	}

	return tools
}

// ProcessEvent routes supported SeaTalk events into the agent dispatcher.
func (a *SeaTalkAgentAdapter) ProcessEvent(ctx context.Context, req seatalk.EventRequest, event seatalk.Event) (any, error) {
	switch e := event.(type) {
	case *seatalk.MessageFromBotSubscriberEvent, *seatalk.NewMentionedMessageReceivedFromGroupChatEvent, *seatalk.NewMessageReceivedFromThreadEvent:
		if a == nil || a.dispatcher == nil {
			return nil, errors.New("process callback event failed: dispatcher is nil")
		}

		route, ok, err := a.router.Route(req, event)
		if err != nil {
			return nil, err
		}
		if !ok {
			log.Printf("seatalk adapter ignored event: event_id=%s type=%T", req.EventID, event)
			return nil, nil
		}
		log.Printf(
			"seatalk adapter accepted event: event_id=%s type=%T conversation=%s kind=%s quoted_message_id=%s has_image=%t has_file=%t has_video=%t",
			req.EventID,
			event,
			route.message.ConversationKey,
			route.message.Kind,
			route.quotedMessageID,
			len(route.imageURLs) > 0,
			len(route.fileAttachments) > 0,
			len(route.videoAttachments) > 0,
		)
		responder := &SeaTalkResponder{
			client:             a.client,
			target:             route.replyTarget,
			typingAllowedUntil: typingAllowedUntil(route.message.SentAtUnix),
		}
		if err = a.populateReplyMention(ctx, responder); err != nil {
			return nil, err
		}
		if !responder.target.mentionTarget.IsZero() {
			route.message.SenderMentionHint = responder.target.mentionTarget.MarkdownTag()
		}
		if err = a.prepareMessageAssets(ctx, &route, responder); err != nil {
			return nil, err
		}
		if privateEvent, ok := e.(*seatalk.MessageFromBotSubscriberEvent); ok {
			a.attachInitialPrivateThreadLoaders(&route, privateEvent, responder)
		}
		if mentionedEvent, ok := e.(*seatalk.NewMentionedMessageReceivedFromGroupChatEvent); ok {
			a.attachInitialMentionedThreadLoaders(&route, mentionedEvent, responder)
		}
		if threadEvent, ok := e.(*seatalk.NewMessageReceivedFromThreadEvent); ok {
			a.attachThreadInitialLoaders(&route, threadEvent, responder)
		}
		route.message.Responder = responder

		if err = a.dispatcher.Enqueue(ctx, route.message); err != nil {
			_ = responder.Cleanup(context.Background()) //nolint:contextcheck
			return nil, err
		}
		log.Printf(
			"seatalk adapter enqueued event: event_id=%s conversation=%s target=%s",
			req.EventID,
			route.message.ConversationKey,
			route.replyTarget.logValue(),
		)
		return nil, nil
	case *seatalk.InteractiveMessageClickEvent:
		log.Printf("seatalk adapter received interactive event: event_id=%s message_id=%s value=%q", req.EventID, e.MessageID, e.Value)
		if err := a.ProcessInteractiveEvent(ctx, req, e); err != nil {
			return nil, fmt.Errorf(
				"process interactive message click event failed: %w. message_id=%s value=%q",
				err,
				e.MessageID,
				e.Value,
			)
		}
		return nil, nil
	case *seatalk.UserEnterChatroomWithBotEvent, *seatalk.BotAddedToGroupChatEvent, *seatalk.BotRemovedFromGroupChatEvent:
		return nil, nil
	default:
		return nil, nil
	}
}

// ProcessInteractiveEvent handles interactive message click events.
func (a *SeaTalkAgentAdapter) ProcessInteractiveEvent(ctx context.Context, req seatalk.EventRequest, event *seatalk.InteractiveMessageClickEvent) error {
	if a == nil || a.dispatcher == nil {
		return errors.New("process interactive event failed: dispatcher is nil")
	}
	if event == nil {
		return errors.New("process interactive event failed: event is nil")
	}

	threadRef := strings.TrimSpace(event.ThreadID)
	if strings.TrimSpace(event.GroupID) == "" {
		threadRef = privateConversationThreadKey(threadRef, event.MessageID)
	} else if threadRef == "" {
		threadRef = strings.TrimSpace(event.MessageID)
	}

	replyThreadID := privateReplyThreadID(event.ThreadID, event.MessageID)
	if strings.TrimSpace(event.GroupID) != "" {
		replyThreadID = threadRef
	}
	if replyThreadID == "" {
		return errors.New("process interactive event failed: thread reference is empty")
	}

	target := seaTalkReplyTarget{
		isGroup:         strings.TrimSpace(event.GroupID) != "",
		groupID:         strings.TrimSpace(event.GroupID),
		employeeCode:    strings.TrimSpace(event.EmployeeCode),
		threadID:        replyThreadID,
		mentionEmployee: strings.TrimSpace(event.EmployeeCode),
		mentionTarget:   newSeaTalkMentionTarget(event.SeatalkID, event.Email),
	}
	if !target.isGroup && target.employeeCode == "" {
		return errors.New("process interactive event failed: employee code is empty")
	}

	responder := &SeaTalkResponder{
		client:             a.client,
		target:             target,
		interactiveMessage: strings.TrimSpace(event.MessageID),
		typingAllowedUntil: typingAllowedUntil(currentMessageUnixTime(0, req.Timestamp)),
	}
	if err := a.populateReplyMention(ctx, responder); err != nil {
		return err
	}

	resolvedValue, err := resolveInteractiveCallbackValue(ctx, event.Value)
	if err != nil {
		return fmt.Errorf("resolve interactive callback value: %w", err)
	}

	callbackAction, err := decodeInteractiveCallbackAction(resolvedValue)
	if err != nil {
		return fmt.Errorf("decode interactive callback action: %w", err)
	}

	messageText := callbackAction.Prompt
	if callbackAction.Action == "tool_call" {
		messageText = buildInteractiveClickMessage(event, resolvedValue)
	}

	message := agent.InboundMessage{
		ID:                req.EventID,
		ConversationKey:   conversationKey(target.isGroup, target.employeeCode, target.groupID, threadRef),
		Kind:              agent.MessageKindText,
		Sender:            formatCurrentMessageSender(event.Email),
		SentAtUnix:        currentMessageUnixTime(0, req.Timestamp),
		SenderMentionHint: responder.target.mentionTarget.MarkdownTag(),
		Text:              messageText,
		Responder:         responder,
	}
	a.attachInteractiveInitialLoaders(&message, event, responder)

	return a.dispatcher.Enqueue(ctx, message)
}

func (a *SeaTalkAgentAdapter) prepareMessageAssets(ctx context.Context, route *seaTalkRoute, responder *SeaTalkResponder) error {
	if route == nil || responder == nil {
		return nil
	}
	if len(route.imageURLs) > 0 && responder.client != nil {
		imagePaths := make([]string, 0, len(route.imageURLs))
		for index, imageURL := range route.imageURLs {
			log.Printf("seatalk adapter preparing inbound image: conversation=%s url=%s", route.message.ConversationKey, imageURL)
			imagePath, err := responder.PrepareImage(ctx, imageURL, "current image")
			if err != nil {
				return err
			}
			if imagePath == "" {
				continue
			}
			if route.message.ImagePath == "" && index == 0 {
				route.message.ImagePath = imagePath
			}
			imagePaths = append(imagePaths, imagePath)
		}
		route.message.ImagePaths = imagePaths
	}
	if len(route.fileAttachments) > 0 && responder.client != nil {
		filePaths := make([]string, 0, len(route.fileAttachments))
		for index, attachment := range route.fileAttachments {
			log.Printf(
				"seatalk adapter preparing inbound file: conversation=%s url=%s filename=%s",
				route.message.ConversationKey,
				attachment.URL,
				attachment.Filename,
			)
			filePath, err := responder.PrepareFile(ctx, attachment.URL, attachment.Filename, "current file")
			if err != nil {
				return err
			}
			if filePath == "" {
				continue
			}
			if route.message.FilePath == "" && index == 0 {
				route.message.FilePath = filePath
			}
			filePaths = append(filePaths, filePath)
		}
		route.message.FilePaths = filePaths
	}
	if len(route.videoAttachments) > 0 && responder.client != nil {
		videoPaths := make([]string, 0, len(route.videoAttachments))
		for index, attachment := range route.videoAttachments {
			log.Printf(
				"seatalk adapter preparing inbound video: conversation=%s url=%s filename=%s",
				route.message.ConversationKey,
				attachment.URL,
				attachment.Filename,
			)
			videoPath, err := responder.PrepareFile(ctx, attachment.URL, attachment.Filename, "current video")
			if err != nil {
				return err
			}
			if videoPath == "" {
				continue
			}
			if route.message.VideoPath == "" && index == 0 {
				route.message.VideoPath = videoPath
			}
			videoPaths = append(videoPaths, videoPath)
		}
		route.message.VideoPaths = videoPaths
	}
	if err := hydrateForwardedReferencedMessages(route.message.ForwardedMessages); err != nil {
		return err
	}
	if len(route.message.ForwardedMessages) == 0 && route.forwardedHistory != nil {
		forwardedMessages, err := buildReferencedForwardedMessages(ctx, responder, route.forwardedHistory)
		if err != nil {
			return err
		}
		route.message.ForwardedMessages = forwardedMessages
	}
	if route.quotedMessageID == "" || route.message.QuotedMessage != nil {
		return nil
	}

	log.Printf(
		"seatalk adapter resolving quoted message: conversation=%s quoted_message_id=%s",
		route.message.ConversationKey,
		route.quotedMessageID,
	)
	quoted, err := a.resolveReferencedMessage(ctx, responder, route.quotedMessageID)
	if err != nil {
		return fmt.Errorf("hydrate quoted message failed: %w", err)
	}
	if quoted == nil {
		return nil
	}

	route.message.QuotedMessage = quoted
	return nil
}

func (a *SeaTalkAgentAdapter) populateReplyMention(ctx context.Context, responder *SeaTalkResponder) error {
	if a == nil || responder == nil || !responder.target.isGroup || !responder.target.mentionTarget.IsZero() || responder.target.mentionEmployee == "" {
		return nil
	}
	if a.client == nil {
		return errors.New("populate reply mention failed: client is nil")
	}

	result, err := a.client.GetEmployeeInfo(ctx, responder.target.mentionEmployee)
	if err != nil {
		if errors.Is(err, seatalk.ErrEmployeeInfoDisabled) {
			return nil
		}
		return fmt.Errorf("populate reply mention failed: %w", err)
	}
	for _, employee := range result.Employees {
		if employee.EmployeeCode != responder.target.mentionEmployee {
			continue
		}
		responder.target.mentionTarget = newSeaTalkMentionTarget(employee.SeatalkID, employee.Email)
		log.Printf(
			"seatalk adapter populated reply mention: target=%s mention_employee=%s mention_seatalk_id=%s mention_email=%s",
			responder.target.logValue(),
			responder.target.mentionEmployee,
			responder.target.mentionTarget.seatalkID,
			responder.target.mentionTarget.email,
		)
		break
	}

	return nil
}

func (a *SeaTalkAgentAdapter) resolveReferencedMessage(ctx context.Context, responder *SeaTalkResponder, messageID string) (*agent.ReferencedMessage, error) {
	if a == nil || a.client == nil {
		return nil, errors.New("resolve referenced message failed: adapter is nil")
	}

	message, err := a.client.GetMessage(ctx, messageID)
	if err != nil {
		return nil, err
	}

	return normalizeQuotedMessage(ctx, responder, message)
}

func (a *SeaTalkAgentAdapter) attachInitialMentionedThreadLoaders(route *seaTalkRoute, event *seatalk.NewMentionedMessageReceivedFromGroupChatEvent, responder *SeaTalkResponder) {
	if a == nil || a.client == nil || route == nil || event == nil || event.GroupID == "" || event.Message.ThreadID == "" {
		return
	}

	groupID := event.GroupID
	threadID := event.Message.ThreadID
	currentMessageID := event.Message.MessageID
	route.message.LoadInitialContext = func(ctx context.Context) (string, error) {
		log.Printf(
			"seatalk adapter loading group thread context: conversation=%s group_id=%s thread_id=%s current_message_id=%s",
			route.message.ConversationKey,
			groupID,
			threadID,
			currentMessageID,
		)
		return a.buildGroupThreadInitialContext(ctx, groupID)
	}
	route.message.LoadInitialMessages = func(ctx context.Context) ([]agent.InboundMessage, error) {
		log.Printf(
			"seatalk adapter loading group thread history from mention event: conversation=%s group_id=%s thread_id=%s current_message_id=%s",
			route.message.ConversationKey,
			groupID,
			threadID,
			currentMessageID,
		)
		return a.buildGroupThreadInitialMessages(ctx, responder, route.message.ConversationKey, groupID, threadID, currentMessageID)
	}
}

func (a *SeaTalkAgentAdapter) attachInitialPrivateThreadLoaders(route *seaTalkRoute, event *seatalk.MessageFromBotSubscriberEvent, responder *SeaTalkResponder) {
	if a == nil || a.client == nil || route == nil || event == nil || event.EmployeeCode == "" {
		return
	}

	threadID := strings.TrimSpace(event.Message.ThreadID)
	if threadID == "" || threadID == "0" {
		return
	}

	employeeCode := event.EmployeeCode
	currentMessageID := event.Message.MessageID
	route.message.LoadInitialContext = func(ctx context.Context) (string, error) {
		log.Printf(
			"seatalk adapter loading private thread context: conversation=%s employee_code=%s thread_id=%s current_message_id=%s",
			route.message.ConversationKey,
			employeeCode,
			threadID,
			currentMessageID,
		)
		return a.buildPrivateThreadInitialContext(ctx, employeeCode)
	}
	route.message.LoadInitialMessages = func(ctx context.Context) ([]agent.InboundMessage, error) {
		log.Printf(
			"seatalk adapter loading private thread history: conversation=%s employee_code=%s thread_id=%s current_message_id=%s",
			route.message.ConversationKey,
			employeeCode,
			threadID,
			currentMessageID,
		)
		return a.buildPrivateThreadInitialMessages(ctx, responder, route.message.ConversationKey, employeeCode, threadID, currentMessageID)
	}
}

func (a *SeaTalkAgentAdapter) attachThreadInitialLoaders(route *seaTalkRoute, event *seatalk.NewMessageReceivedFromThreadEvent, responder *SeaTalkResponder) {
	if a == nil || a.client == nil || route == nil || event == nil {
		return
	}

	threadID := strings.TrimSpace(event.Message.ThreadID)
	currentMessageID := strings.TrimSpace(event.Message.MessageID)
	if threadID == "" || currentMessageID == "" {
		return
	}

	if groupID := strings.TrimSpace(event.GroupID); groupID != "" {
		route.message.LoadInitialContext = appendInitialContextLoader(route.message.LoadInitialContext, func(ctx context.Context) (string, error) {
			log.Printf(
				"seatalk adapter loading group thread context from thread event: conversation=%s group_id=%s thread_id=%s current_message_id=%s",
				route.message.ConversationKey,
				groupID,
				threadID,
				currentMessageID,
			)
			return a.buildGroupThreadInitialContext(ctx, groupID)
		})
		route.message.LoadInitialMessages = appendInitialMessageLoader(route.message.LoadInitialMessages, func(ctx context.Context) ([]agent.InboundMessage, error) {
			log.Printf(
				"seatalk adapter loading group thread history from thread event: conversation=%s group_id=%s thread_id=%s current_message_id=%s",
				route.message.ConversationKey,
				groupID,
				threadID,
				currentMessageID,
			)
			return a.buildGroupThreadInitialMessages(ctx, responder, route.message.ConversationKey, groupID, threadID, currentMessageID)
		})
		return
	}

	employeeCode := strings.TrimSpace(event.Message.Sender.EmployeeCode)
	if employeeCode == "" {
		return
	}

	route.message.LoadInitialContext = appendInitialContextLoader(route.message.LoadInitialContext, func(ctx context.Context) (string, error) {
		log.Printf(
			"seatalk adapter loading private thread context from thread event: conversation=%s employee_code=%s thread_id=%s current_message_id=%s",
			route.message.ConversationKey,
			employeeCode,
			threadID,
			currentMessageID,
		)
		return a.buildPrivateThreadInitialContext(ctx, employeeCode)
	})
	route.message.LoadInitialMessages = appendInitialMessageLoader(route.message.LoadInitialMessages, func(ctx context.Context) ([]agent.InboundMessage, error) {
		log.Printf(
			"seatalk adapter loading private thread history from thread event: conversation=%s employee_code=%s thread_id=%s current_message_id=%s",
			route.message.ConversationKey,
			employeeCode,
			threadID,
			currentMessageID,
		)
		return a.buildPrivateThreadInitialMessages(ctx, responder, route.message.ConversationKey, employeeCode, threadID, currentMessageID)
	})
}

func (a *SeaTalkAgentAdapter) attachInteractiveInitialLoaders(
	message *agent.InboundMessage,
	event *seatalk.InteractiveMessageClickEvent,
	responder *SeaTalkResponder,
) {
	if a == nil || a.client == nil || message == nil || event == nil {
		return
	}

	threadID := strings.TrimSpace(event.ThreadID)
	currentMessageID := strings.TrimSpace(event.MessageID)

	if groupID := strings.TrimSpace(event.GroupID); groupID != "" {
		message.LoadInitialContext = appendInitialContextLoader(message.LoadInitialContext, func(ctx context.Context) (string, error) {
			log.Printf(
				"seatalk adapter loading group thread context from interactive event: conversation=%s group_id=%s thread_id=%s current_message_id=%s",
				message.ConversationKey,
				groupID,
				threadID,
				currentMessageID,
			)
			return a.buildGroupThreadInitialContext(ctx, groupID)
		})
		if threadID == "" {
			return
		}
		message.LoadInitialMessages = appendInitialMessageLoader(message.LoadInitialMessages, func(ctx context.Context) ([]agent.InboundMessage, error) {
			log.Printf(
				"seatalk adapter loading group thread history from interactive event: conversation=%s group_id=%s thread_id=%s current_message_id=%s",
				message.ConversationKey,
				groupID,
				threadID,
				currentMessageID,
			)
			return a.buildGroupThreadInitialMessages(ctx, responder, message.ConversationKey, groupID, threadID, currentMessageID)
		})
		return
	}

	employeeCode := strings.TrimSpace(event.EmployeeCode)
	if employeeCode == "" || threadID == "" || threadID == "0" {
		return
	}

	message.LoadInitialContext = appendInitialContextLoader(message.LoadInitialContext, func(ctx context.Context) (string, error) {
		log.Printf(
			"seatalk adapter loading private thread context from interactive event: conversation=%s employee_code=%s thread_id=%s current_message_id=%s",
			message.ConversationKey,
			employeeCode,
			threadID,
			currentMessageID,
		)
		return a.buildPrivateThreadInitialContext(ctx, employeeCode)
	})
	message.LoadInitialMessages = appendInitialMessageLoader(message.LoadInitialMessages, func(ctx context.Context) ([]agent.InboundMessage, error) {
		log.Printf(
			"seatalk adapter loading private thread history from interactive event: conversation=%s employee_code=%s thread_id=%s current_message_id=%s",
			message.ConversationKey,
			employeeCode,
			threadID,
			currentMessageID,
		)
		return a.buildPrivateThreadInitialMessages(ctx, responder, message.ConversationKey, employeeCode, threadID, currentMessageID)
	})
}

func (a *SeaTalkAgentAdapter) buildGroupThreadInitialContext(ctx context.Context, groupID string) (string, error) {
	if a == nil || a.client == nil {
		return "", errors.New("build group thread initial context failed: adapter is nil")
	}

	log.Printf("seatalk adapter building group initial context: group_id=%s", groupID)
	profile, err := a.loadGroupProfile(ctx, groupID)
	if err != nil {
		return "", err
	}

	lines := []string{
		profile,
		"Group thread guidance:",
		"- The current message may include message tags. The tag `group_mentioned_message` means the bot was explicitly mentioned in that message and should reply.",
		"- For messages without the tag `group_mentioned_message`, reply only when a user-facing response is actually needed.",
	}
	return strings.Join(lines, "\n\n"), nil
}

func (a *SeaTalkAgentAdapter) buildPrivateThreadInitialContext(ctx context.Context, employeeCode string) (string, error) {
	if a == nil || a.client == nil {
		return "", errors.New("build private thread initial context failed: adapter is nil")
	}

	log.Printf("seatalk adapter building private initial context: employee_code=%s", employeeCode)
	return a.loadEmployeeProfile(ctx, employeeCode)
}

func (a *SeaTalkAgentAdapter) loadGroupProfile(ctx context.Context, groupID string) (string, error) {
	result, err := a.client.GetGroupInfo(ctx, groupID, seatalk.GetGroupInfoOptions{
		PageSize: 100,
	})
	if err != nil {
		return "", fmt.Errorf("load group profile failed: %w", err)
	}

	lines := []string{
		"Group profile:",
		"- name: " + fallbackValue(strings.TrimSpace(result.Group.GroupName)),
	}
	if result.Group.GroupUserTotal < 10 {
		if members := formatGroupMembers(result.Group.GroupUserList); members != "" {
			lines = append(lines, "- users:")
			lines = append(lines, members)
		}
	}

	return strings.Join(lines, "\n"), nil
}

func (a *SeaTalkAgentAdapter) loadEmployeeProfile(ctx context.Context, employeeCode string) (string, error) {
	if a == nil {
		return "", nil
	}
	if a.client == nil {
		return "", errors.New("load employee profile failed: client is nil")
	}

	result, err := a.client.GetEmployeeInfo(ctx, employeeCode)
	if err != nil {
		if errors.Is(err, seatalk.ErrEmployeeInfoDisabled) {
			return "", nil
		}
		return "", fmt.Errorf("load employee profile failed: %w", err)
	}
	if len(result.Employees) == 0 {
		return "", fmt.Errorf("load employee profile failed: employee %s not found", employeeCode)
	}

	profile := result.Employees[0]
	lines := []string{
		"Employee profile:",
		"- employee_code: " + fallbackValue(strings.TrimSpace(profile.EmployeeCode)),
		"- email: " + fallbackValue(strings.TrimSpace(profile.Email)),
		"- phone: " + fallbackValue(strings.TrimSpace(profile.Mobile)),
		"- departments: " + joinOrFallback(profile.Departments),
		"- manager_employee_code: " + normalizeManagerCode(profile.ReportingManagerEmployeeCode),
	}

	return strings.Join(lines, "\n"), nil
}

func (a *SeaTalkAgentAdapter) loadGroupThreadMessages(ctx context.Context, groupID, threadID string) ([]seatalk.PrivateThreadMessage, error) {
	var (
		cursor   string
		messages []seatalk.PrivateThreadMessage
	)

	for {
		log.Printf("seatalk adapter fetching group thread page: group_id=%s thread_id=%s cursor=%s", groupID, threadID, cursor)
		result, err := a.client.GetGroupThread(ctx, groupID, threadID, seatalk.GetGroupThreadOptions{
			PageSize: 100,
			Cursor:   cursor,
		})
		if err != nil {
			return nil, fmt.Errorf("load group thread messages failed: %w", err)
		}
		messages = append(messages, result.ThreadMessages...)
		if result.NextCursor == "" {
			return messages, nil
		}
		cursor = result.NextCursor
	}
}

func (a *SeaTalkAgentAdapter) loadPrivateThreadMessages(ctx context.Context, employeeCode, threadID string) ([]seatalk.PrivateThreadMessage, error) {
	var (
		cursor   string
		messages []seatalk.PrivateThreadMessage
	)

	for {
		log.Printf("seatalk adapter fetching private thread page: employee_code=%s thread_id=%s cursor=%s", employeeCode, threadID, cursor)
		result, err := a.client.GetPrivateThread(ctx, employeeCode, threadID, seatalk.GetPrivateThreadOptions{
			PageSize: 100,
			Cursor:   cursor,
		})
		if err != nil {
			return nil, fmt.Errorf("load private thread messages failed: %w", err)
		}
		messages = append(messages, result.ThreadMessages...)
		if result.NextCursor == "" {
			return messages, nil
		}
		cursor = result.NextCursor
	}
}

func (a *SeaTalkAgentAdapter) buildGroupThreadInitialMessages(
	ctx context.Context,
	responder *SeaTalkResponder,
	conversationKey, groupID, threadID, currentMessageID string,
) ([]agent.InboundMessage, error) {
	if a == nil || a.client == nil {
		return nil, errors.New("build group thread initial messages failed: adapter is nil")
	}

	log.Printf("seatalk adapter building group thread history: group_id=%s thread_id=%s current_message_id=%s", groupID, threadID, currentMessageID)
	messages, err := a.loadGroupThreadMessages(ctx, groupID, threadID)
	if err != nil {
		return nil, err
	}

	return a.normalizeThreadHistoryMessages(ctx, responder, conversationKey, messages, currentMessageID)
}

func (a *SeaTalkAgentAdapter) buildPrivateThreadInitialMessages(
	ctx context.Context,
	responder *SeaTalkResponder,
	conversationKey, employeeCode, threadID, currentMessageID string,
) ([]agent.InboundMessage, error) {
	if a == nil || a.client == nil {
		return nil, errors.New("build private thread initial messages failed: adapter is nil")
	}

	log.Printf("seatalk adapter building private thread history: employee_code=%s thread_id=%s current_message_id=%s", employeeCode, threadID, currentMessageID)
	messages, err := a.loadPrivateThreadMessages(ctx, employeeCode, threadID)
	if err != nil {
		return nil, err
	}

	return a.normalizeThreadHistoryMessages(ctx, responder, conversationKey, messages, currentMessageID)
}

func (a *SeaTalkAgentAdapter) normalizeThreadHistoryMessages(
	ctx context.Context,
	responder *SeaTalkResponder,
	conversationKey string,
	messages []seatalk.PrivateThreadMessage,
	currentMessageID string,
) ([]agent.InboundMessage, error) {
	if len(messages) == 0 {
		return nil, nil
	}

	slices.SortFunc(messages, func(left, right seatalk.PrivateThreadMessage) int {
		if left.MessageSentTime != right.MessageSentTime {
			return cmp.Compare(left.MessageSentTime, right.MessageSentTime)
		}
		return strings.Compare(left.MessageID, right.MessageID)
	})

	history := make([]agent.InboundMessage, 0, len(messages))
	for _, message := range messages {
		if currentMessageID != "" && strings.TrimSpace(message.MessageID) == strings.TrimSpace(currentMessageID) {
			continue
		}

		normalized, ok, err := a.normalizeThreadHistoryMessage(ctx, responder, conversationKey, message)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		history = append(history, normalized)
	}

	return history, nil
}

func (a *SeaTalkAgentAdapter) normalizeThreadHistoryMessage(
	ctx context.Context,
	responder *SeaTalkResponder,
	conversationKey string,
	message seatalk.PrivateThreadMessage,
) (agent.InboundMessage, bool, error) {
	textContent := ""
	if message.Tag == string(agent.MessageKindText) && message.Text != nil {
		textContent = expandMentionedText(message.Text.PlainText, message.Text.MentionedList)
	}

	imageURL := ""
	if message.Image != nil {
		imageURL = message.Image.Content
	}

	kind, text, imageURLs, ok := normalizeMessageContent(
		fileAttachmentFromURL(threadFileURL(message.File), threadFilename(message.File)),
		message.Tag,
		textContent,
		imageURL,
		message.InteractiveMessage,
	)
	if !ok {
		return agent.InboundMessage{}, false, nil
	}

	history := agent.InboundMessage{
		ID:              strings.TrimSpace(message.MessageID),
		ConversationKey: conversationKey,
		Kind:            kind,
		Sender:          formatCurrentMessageSender(message.Sender.Email),
		SentAtUnix:      currentMessageUnixTime(message.MessageSentTime, 0),
		Text:            text,
		SenderMentionHint: newSeaTalkMentionTarget(
			message.Sender.SeatalkID,
			message.Sender.Email,
		).MarkdownTag(),
	}

	if len(imageURLs) > 0 && responder != nil && responder.client != nil {
		imagePaths := make([]string, 0, len(imageURLs))
		for index, imageURL := range imageURLs {
			imagePath, err := responder.PrepareImage(ctx, imageURL, "thread history image")
			if err != nil {
				return agent.InboundMessage{}, false, err
			}
			if imagePath == "" {
				continue
			}
			if history.ImagePath == "" && index == 0 {
				history.ImagePath = imagePath
			}
			imagePaths = append(imagePaths, imagePath)
		}
		history.ImagePaths = imagePaths
	}
	if fileURL := threadFileURL(message.File); fileURL != "" && responder != nil && responder.client != nil {
		filePath, err := responder.PrepareFile(ctx, fileURL, threadFilename(message.File), "thread history file")
		if err != nil {
			return agent.InboundMessage{}, false, err
		}
		if filePath != "" {
			history.FilePath = filePath
			history.FilePaths = []string{filePath}
		}
	}
	if videoURL := threadBinaryURL(message.Video); videoURL != "" && responder != nil && responder.client != nil {
		videoPath, err := responder.PrepareFile(ctx, videoURL, "", "thread history video")
		if err != nil {
			return agent.InboundMessage{}, false, err
		}
		if videoPath != "" {
			history.VideoPath = videoPath
			history.VideoPaths = []string{videoPath}
		}
	}
	if err := hydrateForwardedReferencedMessages(history.ForwardedMessages); err != nil {
		return agent.InboundMessage{}, false, err
	}
	if message.CombinedForwardedChatHistory != nil {
		forwardedMessages, err := buildReferencedForwardedMessages(ctx, responder, message.CombinedForwardedChatHistory)
		if err != nil {
			return agent.InboundMessage{}, false, err
		}
		history.ForwardedMessages = forwardedMessages
	}
	if strings.TrimSpace(message.QuotedMessageID) != "" {
		quoted, err := a.resolveReferencedMessage(ctx, responder, message.QuotedMessageID)
		if err != nil {
			return agent.InboundMessage{}, false, err
		}
		history.QuotedMessage = quoted
	}

	return history, true, nil
}

func joinInitialContextBlocks(parts ...string) string {
	blocks := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			blocks = append(blocks, trimmed)
		}
	}
	return strings.Join(blocks, "\n\n")
}

func appendInitialContextLoader(current func(context.Context) (string, error), next func(context.Context) (string, error)) func(context.Context) (string, error) {
	if next == nil {
		return current
	}
	if current == nil {
		return next
	}

	return func(ctx context.Context) (string, error) {
		currentValue, err := current(ctx)
		if err != nil {
			return "", err
		}

		nextValue, err := next(ctx)
		if err != nil {
			return "", err
		}

		return joinInitialContextBlocks(currentValue, nextValue), nil
	}
}

func appendInitialMessageLoader(
	current func(context.Context) ([]agent.InboundMessage, error),
	next func(context.Context) ([]agent.InboundMessage, error),
) func(context.Context) ([]agent.InboundMessage, error) {
	if next == nil {
		return current
	}
	if current == nil {
		return next
	}

	return func(ctx context.Context) ([]agent.InboundMessage, error) {
		currentValue, err := current(ctx)
		if err != nil {
			return nil, err
		}

		nextValue, err := next(ctx)
		if err != nil {
			return nil, err
		}

		merged := make([]agent.InboundMessage, 0, len(currentValue)+len(nextValue))
		merged = append(merged, currentValue...)
		merged = append(merged, nextValue...)
		return merged, nil
	}
}

func hydrateForwardedReferencedMessages(messages []agent.ReferencedMessage) error {
	if len(messages) == 0 {
		return nil
	}

	for index := range messages {
		if err := hydrateForwardedReferencedMessages(messages[index].ForwardedMessages); err != nil {
			return err
		}
	}

	return nil
}

func buildReferencedForwardedMessages(
	ctx context.Context,
	responder *SeaTalkResponder,
	message *seatalk.CombinedForwardedChatHistoryMessage,
) ([]agent.ReferencedMessage, error) {
	drafts := buildForwardedMessageDrafts(message)
	if len(drafts) == 0 {
		return nil, nil
	}

	referenced := make([]agent.ReferencedMessage, 0, len(drafts))
	for _, draft := range drafts {
		current := draft.message
		if len(draft.imageURLs) > 0 && responder != nil && responder.client != nil {
			imagePaths := make([]string, 0, len(draft.imageURLs))
			for imageIndex, imageURL := range draft.imageURLs {
				imagePath, err := responder.PrepareImage(ctx, imageURL, "forwarded image")
				if err != nil {
					return nil, err
				}
				if imagePath == "" {
					continue
				}
				if current.ImagePath == "" && imageIndex == 0 {
					current.ImagePath = imagePath
				}
				imagePaths = append(imagePaths, imagePath)
			}
			current.ImagePaths = imagePaths
		}
		if len(draft.fileAttachments) > 0 && responder != nil && responder.client != nil {
			filePaths := make([]string, 0, len(draft.fileAttachments))
			for fileIndex, attachment := range draft.fileAttachments {
				filePath, err := responder.PrepareFile(ctx, attachment.URL, attachment.Filename, "forwarded file")
				if err != nil {
					return nil, err
				}
				if filePath == "" {
					continue
				}
				if current.FilePath == "" && fileIndex == 0 {
					current.FilePath = filePath
				}
				filePaths = append(filePaths, filePath)
			}
			current.FilePaths = filePaths
		}
		if len(draft.videoAttachments) > 0 && responder != nil && responder.client != nil {
			videoPaths := make([]string, 0, len(draft.videoAttachments))
			for videoIndex, attachment := range draft.videoAttachments {
				videoPath, err := responder.PrepareFile(ctx, attachment.URL, attachment.Filename, "forwarded video")
				if err != nil {
					return nil, err
				}
				if videoPath == "" {
					continue
				}
				if current.VideoPath == "" && videoIndex == 0 {
					current.VideoPath = videoPath
				}
				videoPaths = append(videoPaths, videoPath)
			}
			current.VideoPaths = videoPaths
		}
		if len(current.ForwardedMessages) > 0 {
			if err := hydrateForwardedReferencedMessages(current.ForwardedMessages); err != nil {
				return nil, err
			}
		}
		referenced = append(referenced, current)
	}

	return referenced, nil
}

func fallbackValue(value string) string {
	if value == "" {
		return "unknown"
	}
	return value
}

func joinOrFallback(values []string) string {
	filtered := make([]string, 0, len(values))
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			filtered = append(filtered, trimmed)
		}
	}
	if len(filtered) == 0 {
		return "unknown"
	}
	return strings.Join(filtered, ", ")
}

func normalizeManagerCode(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" || trimmed == "0" {
		return "unknown"
	}
	return trimmed
}

func formatGroupMembers(users []seatalk.GroupUser) string {
	if len(users) == 0 {
		return ""
	}

	lines := make([]string, 0, len(users))
	for _, user := range users {
		identity := fallbackValue(strings.TrimSpace(user.EmployeeCode))
		if email := strings.TrimSpace(user.Email); email != "" {
			identity += " <" + email + ">"
		}
		lines = append(lines, "  - "+identity)
	}

	return strings.Join(lines, "\n")
}

// SeaTalkResponder replies to one SeaTalk conversation target.
type SeaTalkResponder struct {
	client             *seatalk.Client
	target             seaTalkReplyTarget
	tempDir            string
	interactiveMessage string
	typingAllowedUntil time.Time
}

// SendText sends a text reply into the bound conversation target.
func (r *SeaTalkResponder) SendText(ctx context.Context, text string) error {
	if r == nil || r.client == nil {
		return errors.New("send reply failed: responder is nil")
	}
	if text == "" {
		return nil
	}

	log.Printf("seatalk responder sending text: target=%s text_len=%d", r.target.logValue(), len(text))
	if r.target.isGroup {
		content := text
		format := seatalk.TextFormatPlain
		if strings.Contains(text, "<mention-tag ") {
			format = seatalk.TextFormatMarkdown
		}
		_, err := r.client.SendGroupText(ctx, r.target.groupID, seatalk.TextMessage{
			Content: content,
			Format:  format,
		}, seatalk.SendOptions{
			ThreadID: r.target.threadID,
		})
		if err != nil {
			return fmt.Errorf("send group reply failed: %w", err)
		}
		return nil
	}

	_, err := r.client.SendPrivateText(ctx, r.target.employeeCode, seatalk.TextMessage{
		Content: text,
		Format:  seatalk.TextFormatPlain,
	}, seatalk.PrivateSendOptions{
		ThreadID:       r.target.threadID,
		UsablePlatform: seatalk.UsablePlatformAll,
	})
	if err != nil {
		return fmt.Errorf("send private reply failed: %w", err)
	}

	return nil
}

// SetTyping updates the typing indicator for the bound conversation target.
func (r *SeaTalkResponder) SetTyping(ctx context.Context) error {
	if r == nil || r.client == nil {
		return errors.New("set typing failed: responder is nil")
	}
	if !r.typingAllowedUntil.IsZero() && time.Now().After(r.typingAllowedUntil) {
		log.Printf(
			"seatalk responder skipped typing: target=%s deadline=%s",
			r.target.logValue(),
			r.typingAllowedUntil.Format(time.RFC3339),
		)
		return nil
	}

	log.Printf("seatalk responder set typing: target=%s", r.target.logValue())
	if r.target.isGroup {
		return r.client.SetGroupTypingStatus(ctx, r.target.groupID, r.target.threadID)
	}

	return r.client.SetPrivateTypingStatus(ctx, r.target.employeeCode, r.target.threadID)
}

// SendInteractive sends an interactive card into the bound conversation target.
func (r *SeaTalkResponder) SendInteractive(ctx context.Context, message seatalk.InteractiveMessage) (seatalk.SendMessageResult, error) {
	if r == nil || r.client == nil {
		return seatalk.SendMessageResult{}, errors.New("send interactive message failed: responder is nil")
	}

	log.Printf("seatalk responder sending interactive message: element_count=%d", len(message.Elements))
	if r.target.isGroup {
		result, err := r.client.SendGroupInteractive(ctx, r.target.groupID, message, seatalk.SendOptions{
			ThreadID: r.target.threadID,
		})
		if err != nil {
			return seatalk.SendMessageResult{}, fmt.Errorf("send group interactive message failed: %w", err)
		}
		return result, nil
	}

	result, err := r.client.SendPrivateInteractive(ctx, r.target.employeeCode, message, seatalk.PrivateSendOptions{
		ThreadID:       r.target.threadID,
		UsablePlatform: seatalk.UsablePlatformAll,
	})
	if err != nil {
		return seatalk.SendMessageResult{}, fmt.Errorf("send private interactive message failed: %w", err)
	}

	return result, nil
}

// UpdateInteractive updates an interactive card that was previously sent by the bot.
func (r *SeaTalkResponder) UpdateInteractive(ctx context.Context, messageID string, message seatalk.InteractiveMessage) error {
	if r == nil || r.client == nil {
		return errors.New("update interactive message failed: responder is nil")
	}

	if err := r.client.UpdateInteractiveMessage(ctx, messageID, message); err != nil {
		return fmt.Errorf("update interactive message failed: %w", err)
	}

	return nil
}

// CurrentInteractiveMessageID returns the interactive message currently being handled.
func (r *SeaTalkResponder) CurrentInteractiveMessageID() string {
	if r == nil {
		return ""
	}

	return strings.TrimSpace(r.interactiveMessage)
}

// Cleanup releases any per-turn resources held by the responder.
func (r *SeaTalkResponder) Cleanup(context.Context) error {
	if r == nil || r.tempDir == "" {
		return nil
	}

	log.Printf("seatalk responder cleanup temp dir: target=%s temp_dir=%s", r.target.logValue(), r.tempDir)
	if err := os.RemoveAll(r.tempDir); err != nil {
		return fmt.Errorf("cleanup responder temp dir failed: %w", err)
	}
	r.tempDir = ""
	return nil
}

// PrepareImage downloads one SeaTalk-hosted image and stores it in the responder temp directory.
func (r *SeaTalkResponder) PrepareImage(ctx context.Context, imageURL, label string) (string, error) {
	if r == nil || r.client == nil {
		return "", errors.New("prepare image failed: responder is nil")
	}
	if imageURL == "" {
		return "", nil
	}

	log.Printf("seatalk responder downloading image: target=%s label=%s url=%s", r.target.logValue(), label, imageURL)
	tempDir, err := r.ensureTempDir()
	if err != nil {
		return "", err
	}

	content, err := r.client.Download(ctx, imageURL)
	if err != nil {
		return "", fmt.Errorf("prepare %s failed: %w", label, err)
	}

	filePath, err := writeImageTempFile(tempDir, content)
	if err != nil {
		return "", fmt.Errorf("prepare %s failed: %w", label, err)
	}

	return filePath, nil
}

// PrepareFile downloads one SeaTalk-hosted file and stores it in the responder temp directory.
func (r *SeaTalkResponder) PrepareFile(ctx context.Context, fileURL, filename, label string) (string, error) {
	if r == nil || r.client == nil {
		return "", errors.New("prepare file failed: responder is nil")
	}
	if fileURL == "" {
		return "", nil
	}

	log.Printf(
		"seatalk responder downloading file: target=%s label=%s filename=%s url=%s",
		r.target.logValue(),
		label,
		filename,
		fileURL,
	)
	tempDir, err := r.ensureTempDir()
	if err != nil {
		return "", err
	}

	content, err := r.client.Download(ctx, fileURL)
	if err != nil {
		return "", fmt.Errorf("prepare %s failed: %w", label, err)
	}

	filePath, err := writeFileTempFile(tempDir, filename, content)
	if err != nil {
		return "", fmt.Errorf("prepare %s failed: %w", label, err)
	}

	return filePath, nil
}

func (r *SeaTalkResponder) ensureTempDir() (string, error) {
	if r.tempDir != "" {
		return r.tempDir, nil
	}

	tempDir, err := os.MkdirTemp("", "assistant-seatalk-assets-*")
	if err != nil {
		return "", fmt.Errorf("create responder temp dir failed: %w", err)
	}
	r.tempDir = tempDir
	log.Printf("seatalk responder created temp dir: target=%s temp_dir=%s", r.target.logValue(), tempDir)
	return tempDir, nil
}

func typingAllowedUntil(messageSentAtUnix int64) time.Time {
	if messageSentAtUnix <= 0 {
		return time.Time{}
	}

	return time.Unix(messageSentAtUnix, 0).Add(seatalkTypingStatusWindow)
}

func (t seaTalkReplyTarget) logValue() string {
	if t.isGroup {
		return "group:" + t.groupID + ":" + t.threadID
	}

	return "private:" + t.employeeCode + ":" + t.threadID
}

func normalizeQuotedMessage(ctx context.Context, responder *SeaTalkResponder, message seatalk.GetMessageResult) (*agent.ReferencedMessage, error) {
	referenced := &agent.ReferencedMessage{
		Sender:     formatCurrentMessageSender(message.Sender.Email),
		SentAtUnix: currentMessageUnixTime(message.MessageSentTime, 0),
	}

	switch message.Tag {
	case "text":
		if message.Text == nil {
			return nil, nil
		}
		referenced.Kind = agent.MessageKindText
		referenced.Text = strings.TrimSpace(expandMentionedText(message.Text.PlainText, message.Text.MentionedList))
		if referenced.Text == "" {
			return nil, nil
		}
		return referenced, nil
	case "image":
		if message.Image == nil || message.Image.Content == "" {
			return nil, nil
		}
		referenced.Kind = agent.MessageKindImage
		if responder == nil || responder.client == nil {
			return referenced, nil
		}
		imagePath, err := responder.PrepareImage(ctx, message.Image.Content, "quoted image")
		if err != nil {
			return nil, err
		}
		referenced.ImagePath = imagePath
		referenced.ImagePaths = []string{imagePath}
		return referenced, nil
	case "file":
		if message.File == nil || message.File.Content == "" {
			return nil, nil
		}
		referenced.Kind = agent.MessageKindFile
		referenced.Text = strings.TrimSpace(message.File.Filename)
		if responder == nil || responder.client == nil {
			return referenced, nil
		}
		filePath, err := responder.PrepareFile(ctx, message.File.Content, message.File.Filename, "quoted file")
		if err != nil {
			return nil, err
		}
		if filePath != "" {
			referenced.FilePath = filePath
			referenced.FilePaths = []string{filePath}
		}
		return referenced, nil
	case "video":
		if message.Video == nil || message.Video.Content == "" {
			return nil, nil
		}
		referenced.Kind = agent.MessageKindVideo
		if responder == nil || responder.client == nil {
			return referenced, nil
		}
		videoPath, err := responder.PrepareFile(ctx, message.Video.Content, "", "quoted video")
		if err != nil {
			return nil, err
		}
		if videoPath != "" {
			referenced.VideoPath = videoPath
			referenced.VideoPaths = []string{videoPath}
		}
		return referenced, nil
	case "combined_forwarded_chat_history":
		forwardedMessages, err := buildReferencedForwardedMessages(ctx, responder, message.CombinedForwardedChatHistory)
		if err != nil {
			return nil, err
		}
		if len(forwardedMessages) == 0 {
			return nil, nil
		}
		referenced.Kind = agent.MessageKindForwarded
		referenced.ForwardedMessages = forwardedMessages
		return referenced, nil
	case "interactive_message":
		summary := summarizeInteractiveMessage(message.InteractiveMessage)
		imageURLs := seatalk.ExtractInteractiveImageURLs(message.InteractiveMessage)
		imagePaths := make([]string, 0, len(imageURLs))
		if responder != nil && responder.client != nil {
			for _, imageURL := range imageURLs {
				imagePath, err := responder.PrepareImage(ctx, imageURL, "quoted interactive image")
				if err != nil {
					return nil, err
				}
				if imagePath != "" {
					imagePaths = append(imagePaths, imagePath)
				}
			}
		}
		referenced.Kind = agent.MessageKindInteractiveCard
		referenced.Text = summary
		referenced.ImagePaths = imagePaths
		if len(imagePaths) > 0 {
			referenced.ImagePath = imagePaths[0]
		}
		return referenced, nil
	default:
		return nil, nil
	}
}

func writeImageTempFile(dir string, content []byte) (path string, err error) {
	file, err := os.CreateTemp(dir, "image-*"+imageFileExtension(content))
	if err != nil {
		return "", err
	}
	defer func() {
		closeErr := file.Close()
		if err == nil && closeErr != nil {
			err = closeErr
		}
	}()

	if _, err := file.Write(content); err != nil {
		return "", err
	}

	return file.Name(), nil
}

func writeFileTempFile(dir, filename string, content []byte) (path string, err error) {
	file, err := os.CreateTemp(dir, "file-*"+fileExtension(filename, content))
	if err != nil {
		return "", err
	}
	defer func() {
		closeErr := file.Close()
		if err == nil && closeErr != nil {
			err = closeErr
		}
	}()

	if _, err := file.Write(content); err != nil {
		return "", err
	}

	return file.Name(), nil
}

func imageFileExtension(content []byte) string {
	switch http.DetectContentType(content) {
	case "image/png":
		return ".png"
	case "image/jpeg":
		return ".jpg"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	default:
		return ".img"
	}
}

func fileExtension(filename string, content []byte) string {
	if extension := strings.TrimSpace(filepath.Ext(filename)); extension != "" {
		return extension
	}

	contentType := http.DetectContentType(content)
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err == nil {
		contentType = mediaType
	}
	extensions, err := mime.ExtensionsByType(contentType)
	if err == nil && len(extensions) > 0 {
		return extensions[0]
	}

	return ".bin"
}

func threadFileURL(file *seatalk.ThreadFileMessage) string {
	if file == nil {
		return ""
	}
	return strings.TrimSpace(file.Content)
}

func threadBinaryURL(file *seatalk.ThreadBinaryMessage) string {
	if file == nil {
		return ""
	}
	return strings.TrimSpace(file.Content)
}

func threadFilename(file *seatalk.ThreadFileMessage) string {
	if file == nil {
		return ""
	}
	return strings.TrimSpace(file.Filename)
}
