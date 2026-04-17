package service

import (
	"bytes"
	"io"
	"strings"
	stdsync "sync"
)

// logSink converts writer-style output from the sync runner into Log
// events. Buffers partial lines so emitted messages always end at
// newline boundaries.
type logSink struct {
	svc *SyncService
	buf bytes.Buffer
	mu  stdsync.Mutex
}

func newLogSink(svc *SyncService) *logSink {
	return &logSink{svc: svc}
}

func (l *logSink) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.buf.Write(p)
	for {
		i := bytes.IndexByte(l.buf.Bytes(), '\n')
		if i < 0 {
			break
		}
		line := strings.TrimRight(string(l.buf.Bytes()[:i]), "\r")
		l.buf.Next(i + 1)
		if line != "" {
			l.svc.emit(Event{Type: EventLog, Message: line})
		}
	}
	return len(p), nil
}

var _ io.Writer = (*logSink)(nil)
