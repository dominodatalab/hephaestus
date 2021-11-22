package buildkit

import (
	"io"

	"github.com/go-logr/logr"
)

type DiscardCloser struct {
	io.Writer
}

func (DiscardCloser) Close() error { return nil }

type LogWriter struct {
	Logger logr.Logger
}

func (w *LogWriter) Write(msg []byte) (int, error) {
	w.Logger.Info(string(msg))
	return len(msg), nil
}
