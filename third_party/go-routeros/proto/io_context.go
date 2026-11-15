package proto

import (
	"bufio"
	"io"
	"sync"
)

type ctxResult struct {
	num int
	err error
}

type ctxReader struct {
	io.Reader
	done chan struct{}
	once sync.Once
}

func (c *ctxReader) Close() {
	c.once.Do(func() { close(c.done) })
}

func (c *ctxReader) Cancel() {
	c.once.Do(func() { close(c.done) })
}

func (c *ctxReader) Read(p []byte) (int, error) {
	out := make(chan *ctxResult, 1)
	buf := make([]byte, len(p))

	go func() {
		res := new(ctxResult)
		res.num, res.err = c.Reader.Read(buf)
		out <- res
		close(out)
	}()

	select {
	case <-c.done:
		return 0, io.EOF
	case r := <-out:
		copy(p, buf)

		return r.num, r.err
	}
}

type ctxWriter struct {
	*bufio.Writer
	done chan struct{}
	once sync.Once
}

func (c *ctxWriter) Close() {
	c.once.Do(func() { close(c.done) })
}

func (c *ctxWriter) Cancel() {
	c.once.Do(func() { close(c.done) })
}

func (c *ctxWriter) Write(p []byte) (int, error) {
	out := make(chan *ctxResult, 1)
	buf := make([]byte, len(p))
	copy(buf, p)

	go func() {
		res := new(ctxResult)
		res.num, res.err = c.Writer.Write(buf)
		out <- res
		close(out)
	}()

	select {
	case <-c.done:
		return 0, io.EOF
	case r := <-out:
		return r.num, r.err
	}
}
