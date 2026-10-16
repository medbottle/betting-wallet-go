package httpapi

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"
)

// Server wraps http.Server with an explicit listen step, so a bind error
// fails application start instead of surfacing later in a goroutine.
type Server struct {
	srv    *http.Server
	logger *slog.Logger

	ln   net.Listener
	done chan struct{}
	err  error
}

// NewServer builds an unstarted server.
func NewServer(addr string, handler http.Handler, readHeaderTimeout, handlerTimeout time.Duration, logger *slog.Logger) *Server {
	return &Server{
		srv: &http.Server{
			Addr:              addr,
			Handler:           handler,
			ReadHeaderTimeout: readHeaderTimeout,
			// Leave room for the handler timeout to write its own response.
			WriteTimeout: handlerTimeout + 5*time.Second,
			IdleTimeout:  60 * time.Second,
			ErrorLog:     slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
		},
		logger: logger,
	}
}

// Start binds the listener and serves in a background goroutine.
func (s *Server) Start(ctx context.Context) error {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", s.srv.Addr)
	if err != nil {
		return fmt.Errorf("http listen %s: %w", s.srv.Addr, err)
	}
	s.ln = ln
	s.done = make(chan struct{})
	go func() {
		defer close(s.done)
		if err := s.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.err = err
			s.logger.Error("http server stopped unexpectedly", slog.String("error", err.Error()))
		}
	}()
	s.logger.Info("http server listening", slog.String("addr", ln.Addr().String()))
	return nil
}

// Addr returns the bound address. Valid only after Start.
func (s *Server) Addr() net.Addr {
	if s.ln == nil {
		return nil
	}
	return s.ln.Addr()
}

// Shutdown stops accepting connections and waits for in-flight requests
// until ctx expires, then force-closes what is left.
func (s *Server) Shutdown(ctx context.Context) error {
	if s.done == nil {
		return nil
	}
	err := s.srv.Shutdown(ctx)
	if err != nil {
		_ = s.srv.Close()
	}
	<-s.done
	s.logger.Info("http server stopped")
	return errors.Join(err, s.err)
}
