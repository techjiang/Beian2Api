package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"time"
)

var errServerClosed = errors.New("http server closed")

// httpServer 是 http.Server 的薄包装，集中配置超时参数。
type httpServer struct {
	addr    string
	handler http.Handler
	srv     *http.Server
}

func (s *httpServer) ListenAndServe() error {
	s.srv = &http.Server{
		Addr:              s.addr,
		Handler:           s.handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       90 * time.Second,
		MaxHeaderBytes:    1 << 16,
		BaseContext:       func(net.Listener) context.Context { return context.Background() },
	}
	if err := s.srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return errServerClosed
}

func (s *httpServer) Shutdown(ctx context.Context) error {
	if s.srv == nil {
		return nil
	}
	return s.srv.Shutdown(ctx)
}
