package server

import (
	"context"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestRunWaitsForInFlightRequests(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	srv := &http.Server{
		ReadHeaderTimeout: time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			close(started)
			time.Sleep(200 * time.Millisecond)
			_, _ = io.WriteString(w, "done")
		}),
	}
	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- Run(ctx, srv, Options{Listener: ln, ShutdownTimeout: 2 * time.Second}) }()

	body := make(chan string, 1)
	go func() {
		res, err := http.Get("http://" + ln.Addr().String())
		if err != nil {
			body <- "error: " + err.Error()
			return
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		body <- string(b)
	}()
	<-started
	cancel()
	if got := <-body; got != "done" {
		t.Fatalf("in-flight request got %q, want done", got)
	}
	if err := <-runErr; err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestRunTimesOut(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	srv := &http.Server{
		ReadHeaderTimeout: time.Second,
		Handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			close(started)
			<-release
		}),
	}
	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- Run(ctx, srv, Options{Listener: ln, ShutdownTimeout: 100 * time.Millisecond}) }()
	go func() {
		res, err := http.Get("http://" + ln.Addr().String())
		if err == nil {
			res.Body.Close()
		}
	}()
	<-started
	cancel()
	select {
	case err := <-runErr:
		if err == nil {
			t.Fatal("expected a deadline error when requests outlive the shutdown timeout")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after the shutdown timeout")
	}
}
