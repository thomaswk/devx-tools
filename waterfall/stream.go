package waterfall

import (
	"errors"
	"io"
)

var (
	errClosedRead  = errors.New("closed for reading")
	errClosedWrite = errors.New("closed for writing")
)

// Stream defines an interface to send and receive messages.
type Stream interface {
	SendMsg(m interface{}) error
	RecvMsg(m interface{}) error
}

// StreamMessage defines a generic interface to build messages with byte contents
type StreamMessage interface {
	BuildMsg() interface{}
	GetBytes(interface{}) ([]byte, error)
	SetBytes(interface{}, []byte)
	CloseMsg() interface{}
}

// StreamReadWriteCloser wraps arbitrary grpc streams around a ReadWriteCloser implementation.
// Users create a new StreamReadWriteCloser by calling NewReadWriteCloser passing a base stream.
// The stream needs to implement RecvMsg and SendMsg (i.e. ClientStream and ServerStream types),
// And a function to set bytes in the stream message type, get bytes from the message and close the stream.
// NOTE: The reader follows the same semantics as <Server|Client>Stream. It is ok to have concurrent writes
// and reads, but it's not ok to have multiple concurrent reads or multiple concurrent writes.
type StreamReadWriteCloser struct {
	io.ReadWriter
	Stream
	StreamMessage

	lastRead []byte
	msgChan  chan []byte
	errChan  chan error

	cr bool
	cw bool
}

// NewReadWriteCloser returns an initialized StreamReadWriteCloser
func NewReadWriteCloser(stream Stream, sm StreamMessage) *StreamReadWriteCloser {
	rw := &StreamReadWriteCloser{
		Stream:        stream,
		StreamMessage: sm,

		// Avoid blocking when Read is never called
		msgChan: make(chan []byte, 1),
		errChan: make(chan error, 1),
	}
	go rw.startReads()
	return rw
}

// startReads reads from the stream in an out of band goroutine in order to handle overflow reads.
func (s *StreamReadWriteCloser) startReads() {
	for {
		msg := s.BuildMsg()
		err := s.Stream.RecvMsg(msg)
		if err != nil {
			s.errChan <- err
			close(s.msgChan)
			return
		}
		b, err := s.GetBytes(msg)
		if err != nil {
			s.errChan <- err
			close(s.msgChan)
			return
		}
		s.msgChan <- b
	}
}

// Read reads from the underlying stream handling cases where the amount read > len(b).
func (s *StreamReadWriteCloser) Read(b []byte) (int, error) {
	if s.cr {
		return 0, errClosedRead
	}

	if len(s.lastRead) > 0 {
		// we have leftover bytes from last read
		n := copy(b, s.lastRead)
		s.lastRead = s.lastRead[n:]
		return n, nil
	}

	// Try to drain the msg channel before returning in order to fulfill the requested slice.
	nt := 0
	for {
		select {
		case rb, ok := <-s.msgChan:
			if !ok {
				if nt == 0 {
					// The channel was closed and nothing was read
					return 0, <-s.errChan
				}
				// Return what we read and return the error on the next read
				return nt, nil
			}
			n := copy(b[nt:], rb)
			nt += n
			s.lastRead = rb[n:]
			if nt == len(b) {
				return nt, nil
			}
		default:
			return nt, nil
		}
	}
}

// Write writes b to the underlying stream
func (s *StreamReadWriteCloser) Write(b []byte) (int, error) {
	if s.cw {
		return 0, errClosedWrite
	}

	msg := s.BuildMsg()
	s.SetBytes(msg, b)
	if err := s.Stream.SendMsg(msg); err != nil {
		return 0, err
	}
	return len(b), nil
}

// Close closes the the stream
func (s *StreamReadWriteCloser) Close() error {
	s.cr = true
	s.cw = true
	s.CloseRead()
	return s.CloseWrite()
}

// CloseRead closes the read side of the stream.
func (s *StreamReadWriteCloser) CloseRead() error {
	s.cr = true
	return nil
}

// CloseWrite closes the write side of the stream and signals the other side.
func (s *StreamReadWriteCloser) CloseWrite() error {
	s.cw = true
	return s.Stream.SendMsg(s.CloseMsg())
}