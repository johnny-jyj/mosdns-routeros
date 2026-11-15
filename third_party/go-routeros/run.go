package routeros

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/go-routeros/routeros/v3/proto"
)

type asyncReply struct {
	chanReply
	Reply
}

// Run simply calls RunArgs().
func (c *Client) Run(sentences ...string) (*Reply, error) {
	return c.RunArgs(sentences)
}

// RunContext simply calls RunArgsContext().
func (c *Client) RunContext(ctx context.Context, sentences ...string) (*Reply, error) {
	return c.RunArgsContext(ctx, sentences)
}

// RunArgs sends a sentence to the RouterOS device and waits for the reply.
func (c *Client) RunArgs(sentences []string) (*Reply, error) {
	return c.RunArgsContext(context.Background(), sentences)
}

// RunArgsContext sends a sentence to the RouterOS device and waits for the reply.
func (c *Client) RunArgsContext(ctx context.Context, sentences []string) (*Reply, error) {
	c.logger().Debug("RunArgsContext", slog.Any("sentences", sentences))

	if !c.IsAsync() {
		c.w.BeginSentence()
		for _, sentence := range sentences {
			c.w.WriteWord(sentence)
		}
		return c.runArgsContextSync()
	}

	// async mode, assign new tag to request
	tag := c.incrementTag()

	a := &asyncReply{}
	a.reC = make(chan *proto.Sentence)
	a.tag = fmt.Sprintf("r%d", tag)

	// Check the async loop and register the tag before constructing/sending
	// the sentence. Holding mu until the writer lock is acquired prevents an
	// async-loop shutdown from leaving BeginSentence's lock unreleased.
	c.mu.Lock()
	if c.tags == nil {
		c.mu.Unlock()
		return nil, errAsyncLoopEnded
	}

	c.w.BeginSentence()
	for _, sentence := range sentences {
		c.w.WriteWord(sentence)
	}
	c.w.WriteWord(".tag=" + a.tag)
	c.logger().Debug("set tag", slog.String("tag", a.tag))
	c.tags[a.tag] = a
	c.mu.Unlock()

	if err := c.w.EndSentence(); err != nil {
		c.mu.Lock()
		if c.tags != nil {
			delete(c.tags, a.tag)
		}
		c.mu.Unlock()

		return nil, err
	}

	// wait for asyncLoop to close channel or context done
	for {
		select {
		case <-ctx.Done():
			// Abandon only this request: deregister its tag so a late reply
			// is safely ignored by asyncLoop. Do not cancel the shared
			// reader — that would tear down the async loop and fail every
			// other in-flight request on this connection. This is safe for
			// asyncReply because processSentence only accumulates into
			// a.Reply and never sends on a.reC, so nothing can block on the
			// abandoned channel.
			c.mu.Lock()
			if c.tags != nil {
				delete(c.tags, a.tag)
			}
			c.mu.Unlock()

			return nil, ctx.Err()
		case _, ok := <-a.reC:
			if !ok { // channel closed
				return &a.Reply, a.err
			}
		}
	}
}

// runArgsContextSync - read command reply in sync mode and return
func (c *Client) runArgsContextSync() (*Reply, error) {
	var err error
	if err = c.w.EndSentence(); err != nil {
		return nil, err
	}

	out := new(Reply)

	var lastErr error
	for {
		var sen *proto.Sentence

		// read next sentence
		if sen, err = c.r.ReadSentence(); err != nil {
			return nil, err
		}

		var done bool

		switch done, err = out.processSentence(sen); {
		case err != nil && done:
			// processed error sentence and it was fatal
			return nil, err
		case err != nil:
			// processed error sentence, but it was not fatal, read next, store last error
			lastErr = err
		case done:
			// processed sentence is Done, return result and last error
			return out, lastErr
		}
	}
}
