package glog

// Custom log levels https://go.dev/src/log/slog/example_custom_levels_test.go - for fatal & trace

import (
	"context"
	"io"
	"log/slog"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/wx-one/wxingress/internal/logger/levels"
)

// Handler holds all the parameters for the handler
type Handler struct {
	opts  Options
	mu    *sync.Mutex
	out   io.Writer
	attrs []slog.Attr
}

// Options contains the log Level
type Options struct {
	// Level reports the minimum level to log.
	// Levels with lower levels are discarded.
	// If nil, the Handler uses [slog.LevelInfo].
	Level slog.Leveler
}

// New - create a new Handler
func New(out io.Writer, opts *Options) *Handler {
	h := &Handler{out: out, mu: &sync.Mutex{}}
	if opts != nil {
		h.opts = *opts
	}
	if h.opts.Level == nil {
		h.opts.Level = slog.LevelInfo
	}
	return h
}

// Enabled - is this log level enabled?
func (h *Handler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.opts.Level.Level()
}

// WithGroup - not needed
func (h *Handler) WithGroup(_ string) slog.Handler {
	// not needed.
	return h
}

// WithAttrs returns a new Handler with the given attributes appended
func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	newAttrs := make([]slog.Attr, len(h.attrs)+len(attrs))
	copy(newAttrs, h.attrs)
	copy(newAttrs[len(h.attrs):], attrs)
	return &Handler{opts: h.opts, mu: h.mu, out: h.out, attrs: newAttrs}
}

// Handle log event
// Format F20240920 16:53:18.817844   70741 main.go:285] key=value message
//
//	<Level>YYYYMMDD HH:MM:SS.NNNNNN   <pid> <file>:<line>] <key>=<value> <msg>
func (h *Handler) Handle(_ context.Context, r slog.Record) error {
	buf := make([]byte, 0, 1024)
	// LogLevel
	switch r.Level {
	case levels.LevelTrace:
		buf = append(buf, "I"...)
	case levels.LevelDebug:
		buf = append(buf, "I"...)
	case levels.LevelInfo:
		buf = append(buf, "I"...)
	case levels.LevelWarning:
		buf = append(buf, "W"...)
	case levels.LevelError:
		buf = append(buf, "E"...)
	case levels.LevelFatal:
		buf = append(buf, "F"...)
	}

	// date/time
	if !r.Time.IsZero() {
		buf = append(buf, r.Time.Format("20060102 15:04:05.000000")...)
	}

	buf = append(buf, "   "...)

	// PID
	buf = append(buf, strconv.Itoa(os.Getpid())...)

	buf = append(buf, " "...)
	// Log line
	if r.PC != 0 {
		fs := runtime.CallersFrames([]uintptr{r.PC})
		f, _ := fs.Next()
		buf = append(buf, getShortFileName(f.File)...)
		buf = append(buf, ":"...)
		buf = append(buf, strconv.Itoa(f.Line)...)
	}
	buf = append(buf, "]"...)

	// Render handler-level attrs (from WithAttrs)
	for _, a := range h.attrs {
		buf = append(buf, " "...)
		buf = appendEscaped(buf, a.Key)
		buf = append(buf, "="...)
		buf = appendEscaped(buf, a.Value.String())
	}

	// Render record-level attrs (from AddAttrs)
	r.Attrs(func(a slog.Attr) bool {
		buf = append(buf, " "...)
		buf = appendEscaped(buf, a.Key)
		buf = append(buf, "="...)
		buf = appendEscaped(buf, a.Value.String())
		return true
	})

	buf = append(buf, " "...)
	buf = appendEscaped(buf, r.Message)
	buf = append(buf, "\n"...)
	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := h.out.Write(buf)
	return err
}

// appendEscaped appends s with what would end the line or forge another - CR, LF, the other control characters,
// the Unicode line and paragraph separators - escaped as Go does in a quoted string, and invalid UTF-8 as \xNN:
// a message or attribute may carry what a user put into a resource or a request, and one log entry stays one line.
func appendEscaped(buf []byte, s string) []byte {
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && size == 1:
			buf = append(buf, `\x`...)
			buf = append(buf, "0123456789abcdef"[s[i]>>4], "0123456789abcdef"[s[i]&0xf])
		case r == '\n':
			buf = append(buf, `\n`...)
		case r == '\r':
			buf = append(buf, `\r`...)
		case r == '\t':
			buf = append(buf, `\t`...)
		case r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) || r == '\u2028' || r == '\u2029':
			q := strconv.QuoteRuneToASCII(r)
			buf = append(buf, q[1:len(q)-1]...)
		default:
			buf = append(buf, s[i:i+size]...)
		}
		i += size
	}
	return buf
}

func getShortFileName(f string) string {
	fp := strings.Split(f, "/")
	return fp[len(fp)-1]
}
