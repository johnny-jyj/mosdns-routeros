package routeros

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	ros "github.com/go-routeros/routeros/v3"
	"github.com/go-routeros/routeros/v3/proto"
	"go.uber.org/zap"
)

func TestPatchedAsyncHandlesImmediateReplies(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer serverConn.Close()

	c, err := ros.NewClient(clientConn)
	if err != nil {
		t.Fatal(err)
	}
	asyncCtx, asyncCancel := context.WithCancel(context.Background())
	errC := c.AsyncContext(asyncCtx)
	defer func() {
		asyncCancel()
		_ = c.Close()
		<-errC
	}()

	const requests = 500
	serverErr := make(chan error, 1)
	go func() {
		reader := proto.NewReader(serverConn)
		writer := proto.NewWriter(serverConn)
		for i := 0; i < requests; i++ {
			s, err := reader.ReadSentence()
			if err != nil {
				serverErr <- err
				return
			}
			writer.BeginSentence()
			writer.WriteWord("!done")
			writer.WriteWord(".tag=" + s.Tag)
			if err := writer.EndSentence(); err != nil {
				serverErr <- err
				return
			}
		}
		serverErr <- nil
	}()

	for i := 0; i < requests; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		reply, err := c.RunArgsContext(ctx, []string{"/test"})
		cancel()
		if err != nil || reply.Done == nil {
			t.Fatalf("request %d: reply=%v err=%v", i, reply, err)
		}
	}
	if err := <-serverErr; err != nil {
		t.Fatal(err)
	}
}

func TestCommandTimeoutInvalidatesGeneration(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer serverConn.Close()
	c, err := ros.NewClient(clientConn)
	if err != nil {
		t.Fatal(err)
	}
	asyncCtx, asyncCancel := context.WithCancel(context.Background())
	errC := c.AsyncContext(asyncCtx)

	cl := newClient(clientConfig{
		address: "127.0.0.1:1",
		timeout: 20 * time.Millisecond,
	}, zap.NewNop())
	cl.mu.Lock()
	cl.c = c
	cl.cancel = asyncCancel
	cl.gen = 1
	cl.connected.Store(true)
	cl.wg.Add(1)
	cl.mu.Unlock()
	go cl.watch(1, c, asyncCancel, errC)

	read := make(chan struct{})
	go func() {
		reader := proto.NewReader(serverConn)
		_, _ = reader.ReadSentence()
		close(read)
		// Deliberately never send a reply.
	}()

	_, err = cl.run("/hang")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("run error=%v, want deadline exceeded", err)
	}
	<-read
	if cl.Connected() {
		t.Fatal("timed-out connection still reported connected")
	}
	if err := cl.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestPatchedRequestCancellationKeepsSharedReader(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer serverConn.Close()
	c, err := ros.NewClient(clientConn)
	if err != nil {
		t.Fatal(err)
	}
	asyncCtx, asyncCancel := context.WithCancel(context.Background())
	errC := c.AsyncContext(asyncCtx)
	defer func() {
		asyncCancel()
		_ = c.Close()
		<-errC
	}()

	firstRead := make(chan struct{})
	serverErr := make(chan error, 1)
	go func() {
		reader := proto.NewReader(serverConn)
		writer := proto.NewWriter(serverConn)
		if _, err := reader.ReadSentence(); err != nil {
			serverErr <- err
			return
		}
		close(firstRead) // deliberately leave the first request unanswered
		second, err := reader.ReadSentence()
		if err != nil {
			serverErr <- err
			return
		}
		writer.BeginSentence()
		writer.WriteWord("!done")
		writer.WriteWord(".tag=" + second.Tag)
		serverErr <- writer.EndSentence()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	_, err = c.RunArgsContext(ctx, []string{"/first"})
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("first request error=%v", err)
	}
	<-firstRead

	ctx, cancel = context.WithTimeout(context.Background(), time.Second)
	reply, err := c.RunArgsContext(ctx, []string{"/second"})
	cancel()
	if err != nil || reply.Done == nil {
		t.Fatalf("second request: reply=%v err=%v", reply, err)
	}
	if err := <-serverErr; err != nil {
		t.Fatal(err)
	}
}

func TestPatchedAsyncEndedDoesNotLockWriter(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer serverConn.Close()
	c, err := ros.NewClient(clientConn)
	if err != nil {
		t.Fatal(err)
	}
	asyncCtx, asyncCancel := context.WithCancel(context.Background())
	errC := c.AsyncContext(asyncCtx)
	asyncCancel()
	<-errC // tags is now nil

	for i := 0; i < 2; i++ {
		done := make(chan error, 1)
		go func() {
			_, err := c.RunArgsContext(context.Background(), []string{"/after-end"})
			done <- err
		}()
		select {
		case err := <-done:
			if err == nil {
				t.Fatal("command after async end unexpectedly succeeded")
			}
		case <-time.After(100 * time.Millisecond):
			t.Fatalf("command %d blocked on a leaked writer lock", i+1)
		}
	}
	_ = c.Close()
}
