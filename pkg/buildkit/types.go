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

func (w *LogWriter) Read(_ []byte) (n int, err error) {
	return 0, nil
}

func (w *LogWriter) Close() error {
	return nil
}

func (w *LogWriter) Fd() uintptr {
	return 0
}

func (w *LogWriter) Name() string {
	return ""
}

func (w *LogWriter) Write(msg []byte) (int, error) {
	w.Logger.Info(string(msg))
	return len(msg), nil
}
