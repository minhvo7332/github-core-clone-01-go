// Raw TCP HTTP/1.1 transport.
//
// Why not net/http: the timed workloads are thousands of small sequential
// requests, and net/http's per-request machinery is a large share of the
// server's per-request CPU. A hand parser over net.Conn does the same work
// with one parse and one socket write per request, and dispatch is fully
// synchronous (the body is already buffered when the handler runs).
//
// What the suite's clients actually send (undici fetch, Content-Length
// bodies, keep-alive, HTTP/1.1, no chunked uploads, no pipelining): the
// parser handles exactly that, plus chunked bodies defensively.
package main

import (
	"bufio"
	"bytes"
	"errors"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
)

const maxBodyBytes = 64 * 1024 * 1024
const maxHeaderBytes = 64 * 1024

type Request struct {
	Method  string
	Target  string
	Path    string // path part, still percent-encoded, no query
	Query   url.Values
	Headers map[string]string // lower-cased names; duplicates joined ", "
	Body    []byte
}

func (r *Request) header(name string) string { return r.Headers[name] }

type Response struct {
	conn      net.Conn
	buf       *bufio.Writer
	status    int
	headers   [][2]string
	wroteHead bool
}

// WriteHead records the status and headers for the reply.
func (w *Response) WriteHead(status int, headers map[string]string) {
	w.status = status
	w.headers = w.headers[:0]
	for k, v := range headers {
		w.headers = append(w.headers, [2]string{k, v})
	}
	w.wroteHead = true
}

// WriteHeadOrdered is WriteHead with a fixed header order.
func (w *Response) WriteHeadOrdered(status int, headers [][2]string) {
	w.status = status
	w.headers = append(w.headers[:0], headers...)
	w.wroteHead = true
}

var statusText = map[int]string{
	200: "OK", 201: "Created", 204: "No Content", 400: "Bad Request",
	401: "Unauthorized", 404: "Not Found", 405: "Method Not Allowed",
	409: "Conflict", 422: "Unprocessable Entity", 500: "Internal Server Error",
}

var respBufPool = sync.Pool{New: func() any { return new(bytes.Buffer) }}

// End sends the reply: status line + headers + body in one write.
func (w *Response) End(body []byte) {
	if !w.wroteHead {
		w.WriteHead(200, nil)
	}
	buf := respBufPool.Get().(*bytes.Buffer)
	buf.Reset()
	buf.WriteString("HTTP/1.1 ")
	buf.WriteString(strconv.Itoa(w.status))
	buf.WriteByte(' ')
	buf.WriteString(statusText[w.status])
	buf.WriteString("\r\n")
	hasLength := false
	hasConnection := false
	for _, h := range w.headers {
		l := strings.ToLower(h[0])
		if l == "content-length" {
			hasLength = true
		}
		if l == "connection" {
			hasConnection = true
		}
		buf.WriteString(h[0])
		buf.WriteString(": ")
		buf.WriteString(h[1])
		buf.WriteString("\r\n")
	}
	if !hasLength {
		buf.WriteString("content-length: ")
		buf.WriteString(strconv.Itoa(len(body)))
		buf.WriteString("\r\n")
	}
	if !hasConnection {
		buf.WriteString("connection: keep-alive\r\n")
	}
	buf.WriteString("\r\n")
	buf.Write(body)
	w.buf.Write(buf.Bytes())
	w.buf.Flush()
	respBufPool.Put(buf)
}

func splitTarget(target string) (string, url.Values) {
	t := target
	if strings.HasPrefix(t, "http://") || strings.HasPrefix(t, "https://") {
		rest := t[strings.Index(t, "://")+3:]
		slash := strings.IndexByte(rest, '/')
		if slash < 0 {
			t = "/"
		} else {
			t = rest[slash:]
		}
	}
	if q := strings.IndexByte(t, '?'); q >= 0 {
		v, err := url.ParseQuery(t[q+1:])
		if err != nil {
			return t[:q], url.Values{}
		}
		return t[:q], v
	}
	return t, url.Values{}
}

func readChunked(r *bufio.Reader, first []byte) ([]byte, error) {
	// first holds any bytes already buffered past the headers; chunked bodies
	// are rare here, so reassemble through a simple reader.
	br := bufio.NewReader(bytes.NewReader(first))
	_ = br
	var out bytes.Buffer
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if semi := strings.IndexByte(line, ';'); semi >= 0 {
			line = line[:semi]
		}
		size, err := strconv.ParseInt(strings.TrimSpace(line), 16, 64)
		if err != nil || size < 0 {
			return nil, errors.New("bad chunk size")
		}
		if size == 0 {
			// trailer headers until an empty line
			for {
				tl, err := r.ReadString('\n')
				if err != nil {
					return nil, err
				}
				if tl == "\r\n" || tl == "\n" {
					break
				}
			}
			return out.Bytes(), nil
		}
		chunk := make([]byte, size+2)
		if _, err := readFull(r, chunk); err != nil {
			return nil, err
		}
		out.Write(chunk[:size])
	}
}

func readFull(r *bufio.Reader, buf []byte) (int, error) {
	total := 0
	for total < len(buf) {
		n, err := r.Read(buf[total:])
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

type connState struct {
	conn   net.Conn
	reader *bufio.Reader
	writer *bufio.Writer
}

func serveConn(c net.Conn, dispatch func(*Request, *Response)) {
	defer c.Close()
	r := bufio.NewReaderSize(c, 65536)
	w := bufio.NewWriterSize(c, 65536)
	for {
		// Request line.
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			// stray CRLF before a request: tolerate once, like most servers
			continue
		}
		parts := strings.SplitN(line, " ", 3)
		if len(parts) < 3 || parts[0] == "" || parts[1] == "" {
			c.Write([]byte("HTTP/1.1 400 Bad Request\r\ncontent-length: 0\r\nconnection: close\r\n\r\n"))
			return
		}
		method, target := parts[0], parts[1]
		headers := make(map[string]string)
		headerBytes := len(line) + 2
		for {
			hline, err := r.ReadString('\n')
			if err != nil {
				return
			}
			headerBytes += len(hline)
			if headerBytes > maxHeaderBytes {
				c.Write([]byte("HTTP/1.1 400 Bad Request\r\ncontent-length: 0\r\nconnection: close\r\n\r\n"))
				return
			}
			hline = strings.TrimRight(hline, "\r\n")
			if hline == "" {
				break
			}
			colon := strings.IndexByte(hline, ':')
			if colon <= 0 {
				continue
			}
			name := strings.ToLower(strings.TrimSpace(hline[:colon]))
			value := strings.TrimSpace(hline[colon+1:])
			if old, ok := headers[name]; ok {
				headers[name] = old + ", " + value
			} else {
				headers[name] = value
			}
		}
		closeAfter := strings.Contains(strings.ToLower(headers["connection"]), "close")
		if strings.ToLower(headers["expect"]) == "100-continue" {
			c.Write([]byte("HTTP/1.1 100 Continue\r\n\r\n"))
		}
		var body []byte
		if strings.Contains(strings.ToLower(headers["transfer-encoding"]), "chunked") {
			body, err = readChunked(r, nil)
			if err != nil {
				return
			}
		} else {
			cl := 0
			if v := headers["content-length"]; v != "" {
				cl, err = strconv.Atoi(strings.TrimSpace(v))
				if err != nil || cl < 0 {
					c.Write([]byte("HTTP/1.1 400 Bad Request\r\ncontent-length: 0\r\nconnection: close\r\n\r\n"))
					return
				}
			}
			if cl > maxBodyBytes {
				c.Write([]byte("HTTP/1.1 400 Bad Request\r\ncontent-length: 0\r\nconnection: close\r\n\r\n"))
				return
			}
			if cl > 0 {
				body = make([]byte, cl)
				if _, err := readFull(r, body); err != nil {
					return
				}
			}
		}
		path, query := splitTarget(target)
		req := &Request{Method: method, Target: target, Path: path, Query: query, Headers: headers, Body: body}
		res := &Response{conn: c, buf: w}
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					res.WriteHead(500, map[string]string{"content-type": "application/json; charset=utf-8"})
					res.End([]byte(`{"message":"internal error"}`))
				}
			}()
			dispatch(req, res)
		}()
		if closeAfter {
			return
		}
	}
}

// serve runs the accept loop on ln until it closes.
func serve(ln net.Listener, dispatch func(*Request, *Response)) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go serveConn(conn, dispatch)
	}
}
