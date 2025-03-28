package main

import "log/slog"
import "context"

// multiHandler implements slog.Handler to send logs to multiple handlers
type multiHandler struct {
	handlers []slog.Handler
}

// newMultiHandler creates a new handler that sends logs to multiple destinations
func newMultiHandler(handlers ...slog.Handler) *multiHandler {
	return &multiHandler{handlers: handlers}
}

// Enabled implements slog.Handler
func (h *multiHandler) Enabled(ctx context.Context, level slog.Level) bool {
	// The handler is enabled if any of its target handlers are enabled
	for _, handler := range h.handlers {
		if handler.Enabled(ctx, level) {
			return true
		}
	}
	return false
}

// Handle implements slog.Handler
func (h *multiHandler) Handle(ctx context.Context, r slog.Record) error {
	// Send the record to all handlers
	for _, handler := range h.handlers {
		if err := handler.Handle(ctx, r.Clone()); err != nil {
			return err
		}
	}
	return nil
}

// WithAttrs implements slog.Handler
func (h *multiHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	handlers := make([]slog.Handler, len(h.handlers))
	for i, handler := range h.handlers {
		handlers[i] = handler.WithAttrs(attrs)
	}
	return &multiHandler{handlers: handlers}
}

// WithGroup implements slog.Handler
func (h *multiHandler) WithGroup(name string) slog.Handler {
	handlers := make([]slog.Handler, len(h.handlers))
	for i, handler := range h.handlers {
		handlers[i] = handler.WithGroup(name)
	}
	return &multiHandler{handlers: handlers}
}
