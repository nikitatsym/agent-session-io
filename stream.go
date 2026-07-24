package sessionio

import (
	"context"
	"errors"
	"io"
	"sync"
)

// ErrStreamClosed is returned when Next is called after Close.
var ErrStreamClosed = errors.New("sessionio: stream closed")

// Stream is a pull-based sequence with explicit resource ownership.
type Stream[T any] interface {
	Next(context.Context) (T, error)
	Close() error
}

// NextFunc produces the next stream value.
type NextFunc[T any] func(context.Context) (T, error)

// NewStream wraps callbacks in the Stream lifecycle contract.
func NewStream[T any](next NextFunc[T], close func() error) (Stream[T], error) {
	if next == nil {
		return nil, errors.New("sessionio: stream next callback must not be nil")
	}
	if close == nil {
		return nil, errors.New("sessionio: stream close callback must not be nil")
	}
	return &callbackStream[T]{
		next:  next,
		close: close,
	}, nil
}

type callbackStream[T any] struct {
	mu          sync.Mutex
	next        NextFunc[T]
	close       func() error
	closed      bool
	exhausted   bool
	closeResult error
}

func (stream *callbackStream[T]) Next(ctx context.Context) (T, error) {
	stream.mu.Lock()
	defer stream.mu.Unlock()

	var zero T
	if stream.closed {
		return zero, ErrStreamClosed
	}
	if stream.exhausted {
		return zero, io.EOF
	}
	if ctx == nil {
		return zero, errors.New("sessionio: stream context must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return zero, err
	}

	value, err := stream.next(ctx)
	if errors.Is(err, io.EOF) {
		stream.exhausted = true
	}
	return value, err
}

func (stream *callbackStream[T]) Close() error {
	stream.mu.Lock()
	defer stream.mu.Unlock()

	if stream.closed {
		return stream.closeResult
	}
	stream.closed = true
	stream.closeResult = stream.close()
	return stream.closeResult
}
