package seatalk

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
)

var (
	ErrUnknownEventType = errors.New("unknown event type")
	ErrInvalidSignature = errors.New("invalid signature")
	ErrAccessDenied     = errors.New("access denied")
)

// EventProcessor handles one verified non-verification callback event.
type EventProcessor interface {
	ProcessEvent(ctx context.Context, req EventRequest, event Event) (respBody any, err error)
}

// CalculateSignature computes the callback signature from the raw request body and app secret.
func CalculateSignature(body []byte, appSecret string) string {
	hasher := sha256.New()
	_, _ = hasher.Write(body)
	_, _ = hasher.Write([]byte(appSecret))
	return hex.EncodeToString(hasher.Sum(nil))
}

// SignatureFromHeader returns the callback signature from the Signature header.
func SignatureFromHeader(header http.Header) string {
	return strings.ToLower(strings.TrimSpace(header.Get("Signature")))
}

// VerifySignature checks whether the request signature matches the raw body and app secret.
func VerifySignature(header http.Header, body []byte, appSecret string) error {
	got := SignatureFromHeader(header)
	if got == "" {
		log.Printf("verify signature failed: missing signature header")
		return ErrInvalidSignature
	}

	want := CalculateSignature(body, appSecret)
	if got != want {
		log.Printf("verify signature failed: signature mismatch")
		return ErrInvalidSignature
	}

	return nil
}

// EventRequest is the common envelope used by SeaTalk event callbacks.
type EventRequest struct {
	EventID   string          `json:"event_id"`
	EventType string          `json:"event_type"`
	Timestamp int64           `json:"timestamp"`
	AppID     string          `json:"app_id"`
	Event     json.RawMessage `json:"event"`
}

// ParseEventRequest decodes an event envelope from JSON.
func ParseEventRequest(data []byte) (EventRequest, error) {
	var req EventRequest
	if err := json.Unmarshal(data, &req); err != nil {
		log.Printf("parse event request failed: %v", err)
		return EventRequest{}, err
	}
	return req, nil
}

// decodeEvent decodes the embedded event payload into its concrete event type.
func (r EventRequest) decodeEvent() (Event, error) {
	factory, ok := eventFactories[r.EventType]
	if !ok {
		err := fmt.Errorf("%w: %s", ErrUnknownEventType, r.EventType)
		log.Printf("event processing failed: event_id=%s: unsupported event type: event_type=%s", r.EventID, r.EventType)
		return nil, err
	}

	event := factory()
	if err := json.Unmarshal(r.Event, event); err != nil {
		log.Printf("event processing failed: event_id=%s: decode event payload failed: event_type=%s: %v", r.EventID, r.EventType, err)
		return nil, err
	}

	return event, nil
}

// CallbackHandler handles SeaTalk event callbacks.
type CallbackHandler struct {
	cfg Config
	// processor handles verified non-verification callback events.
	processor EventProcessor
}

// NewCallbackHandler builds a SeaTalk callback handler.
func NewCallbackHandler(cfg Config, processor EventProcessor) *CallbackHandler {
	return &CallbackHandler{
		cfg:       cfg,
		processor: processor,
	}
}

// ServeHTTP validates, decodes and processes a SeaTalk callback request.
func (h *CallbackHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	reqBody, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read request body failed", http.StatusBadRequest)
		return
	}

	req, err := h.parseRequest(r, reqBody)
	if err != nil {
		h.writeParseError(w, err)
		return
	}

	h.handleRequest(r.Context(), w, req)
}

func (h *CallbackHandler) parseRequest(r *http.Request, reqBody []byte) (EventRequest, error) {
	if err := VerifySignature(r.Header, reqBody, h.cfg.SigningSecret); err != nil {
		return EventRequest{}, err
	}

	req, err := ParseEventRequest(reqBody)
	if err != nil {
		return EventRequest{}, err
	}
	if req.AppID != h.cfg.AppID {
		return req, ErrAccessDenied
	}

	return req, nil
}

func (h *CallbackHandler) handleRequest(ctx context.Context, w http.ResponseWriter, req EventRequest) {
	event, err := req.decodeEvent()
	if err != nil {
		if errors.Is(err, ErrUnknownEventType) {
			log.Printf("unknown event type: %s", req.EventType)
			http.Error(w, "invalid event_type", http.StatusBadRequest)
			return
		}
		http.Error(w, "invalid event body", http.StatusBadRequest)
		return
	}

	log.Printf("received event: event_id=%s event_type=%s %s", req.EventID, event.EventType(), event.String())

	respBody, err := h.processEvent(ctx, req, event)
	if err == nil {
		if respBody == nil {
			respBody = struct{}{}
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(respBody); err != nil {
			log.Printf("write response failed: %v", err)
		}
		return
	}

	if errors.Is(err, ErrUnknownEventType) {
		log.Printf("unknown event type: %s", req.EventType)
		http.Error(w, "invalid event_type", http.StatusBadRequest)
		return
	}

	http.Error(w, "invalid event body", http.StatusBadRequest)
}

func (h *CallbackHandler) processEvent(ctx context.Context, req EventRequest, event Event) (any, error) {
	if verificationEvent, ok := event.(*VerificationEvent); ok {
		return VerificationResponse(verificationEvent), nil
	}

	if h.processor == nil {
		log.Printf("event processing skipped: event_id=%s event_type=%s: no processor configured", req.EventID, req.EventType)
		return nil, nil
	}

	respBody, err := h.processor.ProcessEvent(ctx, req, event)
	if err != nil {
		log.Printf("event processing failed: event_id=%s: process event failed: event_type=%s: %v", req.EventID, req.EventType, err)
		return nil, err
	}

	return respBody, nil
}

func (h *CallbackHandler) writeParseError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrAccessDenied):
		http.Error(w, "access denied", http.StatusForbidden)
	case errors.Is(err, ErrInvalidSignature):
		http.Error(w, "invalid signature", http.StatusForbidden)
	case err != nil:
		http.Error(w, "invalid json body", http.StatusBadRequest)
	}
}
