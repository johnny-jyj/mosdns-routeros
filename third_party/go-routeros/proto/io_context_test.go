package proto

import (
	"io"
	"testing"
	"time"
)

type blockingReader struct{ release chan struct{} }

func (r *blockingReader) Read([]byte) (int, error) {
	<-r.release
	return 0, io.EOF
}

func TestReaderCancelBeforeReadIsNotLost(t *testing.T) {
	underlying := &blockingReader{release: make(chan struct{})}
	defer close(underlying.release)
	r := &ctxReader{Reader: underlying, done: make(chan struct{})}

	// AsyncContext cancellation can win the race with asyncLoop's first read.
	// Cancellation must remain observable instead of being a best-effort send
	// that is dropped because no Read is selecting on done yet.
	r.Cancel()

	readDone := make(chan error, 1)
	go func() {
		_, err := r.Read(make([]byte, 1))
		readDone <- err
	}()
	select {
	case err := <-readDone:
		if err != io.EOF {
			t.Fatalf("Read error=%v, want EOF", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Cancel before Read was lost")
	}
}
