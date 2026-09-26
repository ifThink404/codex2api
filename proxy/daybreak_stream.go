package proxy

import (
	"bytes"
	"io"
)

const daybreakEventLineLimit = 64 << 10

// 旁路观察错误事件，不改变流内容；超长非错误事件不留存。
type daybreakEventObserver struct {
	io.ReadCloser
	attempt  daybreakAttempt
	line     []byte
	dropping bool
}

func (observer *daybreakEventObserver) Read(buffer []byte) (int, error) {
	n, err := observer.ReadCloser.Read(buffer)
	remaining := buffer[:n]
	for len(remaining) > 0 {
		end := bytes.IndexByte(remaining, '\n')
		if end < 0 {
			observer.appendLine(remaining)
			break
		}
		observer.appendLine(remaining[:end])
		if !observer.dropping {
			observer.inspectLine()
		}
		observer.line, observer.dropping = observer.line[:0], false
		remaining = remaining[end+1:]
	}
	if err == io.EOF && !observer.dropping {
		observer.inspectLine()
	}
	return n, err
}

func (observer *daybreakEventObserver) appendLine(part []byte) {
	if len(observer.line)+len(part) > daybreakEventLineLimit {
		observer.dropping = true
		observer.line = nil
	}
	if !observer.dropping {
		observer.line = append(observer.line, part...)
	}
}

func (observer *daybreakEventObserver) inspectLine() {
	if bytes.HasPrefix(observer.line, []byte("data:")) {
		observer.attempt.inspect(bytes.TrimSpace(observer.line[len("data:"):]))
	}
}
