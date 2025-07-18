package tio

import (
	"errors"
	"io"
	"net"
	"strings"
	"time"

	"github.com/richardwilkes/toolbox/v2/errs"
)

// ReadWithDeadline reads a buffer from a connection with a deadline.
func ReadWithDeadline(conn net.Conn, buffer []byte, deadline time.Duration) error {
	if deadline > 0 {
		if err := conn.SetReadDeadline(time.Now().Add(deadline)); err != nil {
			return errs.Wrap(err)
		}
	}
	_, err := io.ReadFull(conn, buffer)
	return errs.Wrap(err)
}

// WriteWithDeadline writes a buffer to a connection with a deadline.
func WriteWithDeadline(conn net.Conn, buffer []byte, deadline time.Duration) error {
	if deadline > 0 {
		if err := conn.SetWriteDeadline(time.Now().Add(deadline)); err != nil {
			return errs.Wrap(err)
		}
	}
	_, err := conn.Write(buffer)
	return errs.Wrap(err)
}

// ShouldLogIOErrorOriginal returns true if the error should be logged. (kept for use later)
func ShouldLogIOErrorOriginal(err error) bool {
	if err == nil || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return false
	}
	msg := err.Error()
	for _, ignore := range []string{
		"use of closed network connection",
		"operation timed out",
		"connection reset by peer",
		"i/o timeout",
		"connection refused",
	} {
		if strings.Contains(msg, ignore) {
			return false
		}
	}
	return true
}

// ShouldLogIOError returns true if the error should be logged.
func ShouldLogIOError(err error) bool {
	return err != nil && !errors.Is(err, io.EOF)
}
