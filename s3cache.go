package main

import (
	"bytes"
	"net/http"
	"sync"
	"time"
)

var cdnCache sync.Map // map[string]*cacheEntry

type cacheEntry struct {
	Body       []byte
	Headers    http.Header
	StatusCode int
	ExpiresAt  time.Time
}

type cachingResponseWriter struct {
	http.ResponseWriter
	buf        bytes.Buffer
	statusCode int
}

func (w *cachingResponseWriter) WriteHeader(code int) {
	w.statusCode = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *cachingResponseWriter) Write(b []byte) (int, error) {
	w.buf.Write(b)
	return w.ResponseWriter.Write(b)
}

type trackingResponseWriter struct {
	http.ResponseWriter
	statusCode int
	bytes      int64
}

func (w *trackingResponseWriter) WriteHeader(code int) {
	w.statusCode = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *trackingResponseWriter) Write(b []byte) (int, error) {
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, err
}
