package main

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// icapServer implements a minimal ICAP server for response modification.
type icapServer struct {
	addr    string
	handler func(domain string, httpResp *http.Response) (*http.Response, error)
}

func newICAPServer(addr string, handler func(string, *http.Response) (*http.Response, error)) *icapServer {
	return &icapServer{addr: addr, handler: handler}
}

func (s *icapServer) ListenAndServe() error {
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return err
	}
	defer ln.Close()
	log.Printf("ICAP server listening on %s", s.addr)

	for {
		conn, err := ln.Accept()
		if err != nil {
			log.Printf("Accept error: %v", err)
			continue
		}
		go s.handleConn(conn)
	}
}

func (s *icapServer) handleConn(conn net.Conn) {
	defer conn.Close()
	reader := bufio.NewReader(conn)

	for {
		reqLine, err := reader.ReadString('\n')
		if err != nil {
			if err != io.EOF {
				log.Printf("[CONN] Read error: %v", err)
			}
			return
		}
		reqLine = strings.TrimSpace(reqLine)
		if reqLine == "" {
			continue
		}

		parts := strings.Fields(reqLine)
		if len(parts) < 3 {
			log.Printf("[ICAP] Malformed request line: %q", reqLine)
			return
		}
		method := parts[0]

		// Read ICAP headers
		headers := make(map[string]string)
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimSpace(line)
			if line == "" {
				break
			}
			if idx := strings.Index(line, ":"); idx > 0 {
				key := strings.TrimSpace(line[:idx])
				val := strings.TrimSpace(line[idx+1:])
				headers[key] = val
			}
		}

		switch method {
		case "OPTIONS":
			s.handleOptions(conn)
		case "RESPMOD":
			s.handleRespmod(conn, reader, headers)
		default:
			writeICAPError(conn, "405", "Method Not Allowed")
		}
	}
}

func (s *icapServer) handleOptions(conn net.Conn) {
	resp := "ICAP/1.0 200 OK\r\n" +
		"Methods: RESPMOD\r\n" +
		"Allow: 204\r\n" +
		"Preview: 0\r\n" +
		"Transfer-Preview: *\r\n" +
		"ISTag: \"TranslateProxy-1.0\"\r\n" +
		"Encapsulated: null-body=0\r\n" +
		"\r\n"
	conn.Write([]byte(resp))
}

func (s *icapServer) handleRespmod(conn net.Conn, reader *bufio.Reader, headers map[string]string) {
	encap := headers["Encapsulated"]
	if encap == "" {
		log.Println("[RESPMOD] Missing Encapsulated header")
		writeICAPNoContent(conn)
		return
	}

	isPreview := headers["Preview"] != ""
	log.Printf("[RESPMOD] Encapsulated: %s, Preview: %q", encap, headers["Preview"])

	sections := parseEncapsulated(encap)
	sort.Slice(sections, func(i, j int) bool {
		return sections[i].offset < sections[j].offset
	})

	// Calculate section byte lengths from offset differences
	type sectionData struct {
		name   string
		length int
	}
	var sectionLengths []sectionData
	for i, sec := range sections {
		var length int
		if i+1 < len(sections) {
			length = sections[i+1].offset - sec.offset
		}
		sectionLengths = append(sectionLengths, sectionData{name: sec.name, length: length})
	}

	var reqHdrBytes, resHdrBytes []byte
	hasBody := false

	for _, sec := range sectionLengths {
		switch sec.name {
		case "req-hdr":
			if sec.length > 0 {
				reqHdrBytes = make([]byte, sec.length)
				if _, err := io.ReadFull(reader, reqHdrBytes); err != nil {
					log.Printf("[RESPMOD] Error reading req-hdr: %v", err)
					writeICAPNoContent(conn)
					return
				}
				log.Printf("[RESPMOD] Read %d bytes of req-hdr", len(reqHdrBytes))
			}
		case "res-hdr":
			if sec.length > 0 {
				resHdrBytes = make([]byte, sec.length)
				if _, err := io.ReadFull(reader, resHdrBytes); err != nil {
					log.Printf("[RESPMOD] Error reading res-hdr: %v", err)
					writeICAPNoContent(conn)
					return
				}
				log.Printf("[RESPMOD] Read %d bytes of res-hdr", len(resHdrBytes))
			}
		case "res-body":
			hasBody = true
		case "null-body":
			hasBody = false
		}
	}

	domain := extractDomain(string(reqHdrBytes))
	log.Printf("[RESPMOD] Domain: %s", domain)

	if len(resHdrBytes) == 0 {
		log.Println("[RESPMOD] No response headers found")
		writeICAPNoContent(conn)
		return
	}

	// Parse the HTTP response headers
	httpResp, err := http.ReadResponse(bufio.NewReader(strings.NewReader(string(resHdrBytes))), nil)
	if err != nil {
		log.Printf("[RESPMOD] Error parsing HTTP response: %v", err)
		writeICAPNoContent(conn)
		return
	}

	contentType := httpResp.Header.Get("Content-Type")
	log.Printf("[RESPMOD] Content-Type: %s, HasBody: %v", contentType, hasBody)

	if !hasBody {
		log.Println("[RESPMOD] No body, sending 204")
		writeICAPNoContent(conn)
		return
	}

	// Read preview body (which may be 0 bytes)
	previewBody, err := readChunkedBody(reader)
	if err != nil {
		log.Printf("[RESPMOD] Error reading preview/body: %v", err)
		writeICAPNoContent(conn)
		return
	}
	log.Printf("[RESPMOD] Read %d bytes of preview/body", len(previewBody))

	if isPreview {
		// This was a preview. We need to decide: 100 Continue or 204 No Content.
		// For text content, send 100 Continue to get the full body.
		if !isTextContent(contentType) {
			log.Printf("[RESPMOD] Non-text content (%s), sending 204", contentType)
			writeICAPNoContent(conn)
			return
		}

		// Send 100 Continue to get the full body
		log.Println("[RESPMOD] Text content detected, sending 100 Continue")
		conn.Write([]byte("ICAP/1.0 100 Continue\r\n\r\n"))

		// Read the full body
		fullBody, err := readChunkedBody(reader)
		if err != nil {
			log.Printf("[RESPMOD] Error reading full body after continue: %v", err)
			writeICAPNoContent(conn)
			return
		}

		// Combine preview + full body
		previewBody = append(previewBody, fullBody...)
		log.Printf("[RESPMOD] Total body size: %d bytes", len(previewBody))
	}

	if len(previewBody) == 0 {
		log.Println("[RESPMOD] Empty body, sending 204")
		writeICAPNoContent(conn)
		return
	}

	// Set body on the response and call the handler
	sentContinue := isPreview && isTextContent(contentType)
	httpResp.Body = io.NopCloser(strings.NewReader(string(previewBody)))

	modResp, err := s.handler(domain, httpResp)
	if err != nil || modResp == nil {
		if sentContinue {
			// After 100 Continue, we MUST respond with 200, not 204
			log.Println("[RESPMOD] No modification needed, echoing original (post-Continue)")
			sendICAPEcho(conn, resHdrBytes, previewBody)
			return
		}
		log.Println("[RESPMOD] Handler returned nil (no modification)")
		writeICAPNoContent(conn)
		return
	}

	// Read modified body
	modBody, err := io.ReadAll(modResp.Body)
	if err != nil {
		log.Printf("[RESPMOD] Error reading modified body: %v", err)
		if sentContinue {
			sendICAPEcho(conn, resHdrBytes, previewBody)
		} else {
			writeICAPNoContent(conn)
		}
		return
	}

	log.Printf("[RESPMOD] Sending modified response (%d bytes)", len(modBody))

	// Build the modified HTTP response header block
	modResp.Header.Set("Content-Length", strconv.Itoa(len(modBody)))
	modResp.Header.Del("Content-Encoding")
	modResp.Header.Del("Transfer-Encoding")
	modResp.Header.Set("X-TranslateProxy", "translated")
	modResp.ContentLength = int64(len(modBody))

	var httpHdrBuf strings.Builder
	fmt.Fprintf(&httpHdrBuf, "HTTP/%d.%d %s\r\n", modResp.ProtoMajor, modResp.ProtoMinor, modResp.Status)
	for key, vals := range modResp.Header {
		for _, val := range vals {
			fmt.Fprintf(&httpHdrBuf, "%s: %s\r\n", key, val)
		}
	}
	httpHdrBuf.WriteString("\r\n")
	httpHdrStr := httpHdrBuf.String()

	// Build the full ICAP response
	var buf strings.Builder
	fmt.Fprintf(&buf, "ICAP/1.0 200 OK\r\n")
	fmt.Fprintf(&buf, "ISTag: \"TranslateProxy-1.0\"\r\n")
	fmt.Fprintf(&buf, "Encapsulated: res-hdr=0, res-body=%d\r\n", len(httpHdrStr))
	buf.WriteString("\r\n")
	buf.WriteString(httpHdrStr)
	fmt.Fprintf(&buf, "%x\r\n", len(modBody))
	buf.Write(modBody)
	buf.WriteString("\r\n0\r\n\r\n")

	conn.Write([]byte(buf.String()))
	log.Println("[RESPMOD] Response sent successfully")
}

type encapSection struct {
	name   string
	offset int
}

func parseEncapsulated(value string) []encapSection {
	var sections []encapSection
	parts := strings.Split(value, ",")
	for _, part := range parts {
		part = strings.TrimSpace(part)
		kv := strings.SplitN(part, "=", 2)
		if len(kv) != 2 {
			continue
		}
		offset, err := strconv.Atoi(strings.TrimSpace(kv[1]))
		if err != nil {
			continue
		}
		sections = append(sections, encapSection{name: strings.TrimSpace(kv[0]), offset: offset})
	}
	return sections
}

func readChunkedBody(reader *bufio.Reader) ([]byte, error) {
	var body []byte
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return body, err
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		// Parse chunk size (may include extensions after semicolon)
		sizeStr := strings.SplitN(line, ";", 2)[0]
		size, err := strconv.ParseInt(strings.TrimSpace(sizeStr), 16, 64)
		if err != nil {
			return body, fmt.Errorf("invalid chunk size %q: %w", line, err)
		}
		if size == 0 {
			// Read optional trailing CRLF
			reader.ReadString('\n')
			break
		}

		chunk := make([]byte, size)
		_, err = io.ReadFull(reader, chunk)
		if err != nil {
			return body, fmt.Errorf("reading chunk data: %w", err)
		}
		body = append(body, chunk...)

		// Read trailing CRLF after chunk data
		reader.ReadString('\n')
	}
	return body, nil
}

func writeICAPNoContent(conn net.Conn) {
	conn.Write([]byte("ICAP/1.0 204 No Content\r\nISTag: \"TranslateProxy-1.0\"\r\nEncapsulated: null-body=0\r\n\r\n"))
}

func writeICAPError(conn net.Conn, code string, reason string) {
	resp := fmt.Sprintf("ICAP/1.0 %s %s\r\nEncapsulated: null-body=0\r\n\r\n", code, reason)
	conn.Write([]byte(resp))
}

// sendICAPEcho sends a 200 OK with the original (unmodified) response.
// Used when we've already sent 100 Continue and can't use 204.
func sendICAPEcho(conn net.Conn, resHdrBytes []byte, body []byte) {
	var buf strings.Builder
	fmt.Fprintf(&buf, "ICAP/1.0 200 OK\r\n")
	fmt.Fprintf(&buf, "ISTag: \"TranslateProxy-1.0\"\r\n")
	fmt.Fprintf(&buf, "Encapsulated: res-hdr=0, res-body=%d\r\n", len(resHdrBytes))
	buf.WriteString("\r\n")
	buf.Write(resHdrBytes)
	fmt.Fprintf(&buf, "%x\r\n", len(body))
	buf.Write(body)
	buf.WriteString("\r\n0\r\n\r\n")
	conn.Write([]byte(buf.String()))
}

// extractDomain parses the domain from HTTP request headers.
func extractDomain(reqHeaders string) string {
	lines := strings.Split(reqHeaders, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(strings.ToLower(line), "host:") {
			return strings.TrimSpace(line[5:])
		}
	}
	// Try to extract from request line (GET http://host/path HTTP/1.1)
	if len(lines) > 0 {
		parts := strings.Fields(lines[0])
		if len(parts) >= 2 {
			if strings.Contains(parts[1], "://") {
				parts[1] = strings.SplitN(parts[1], "://", 2)[1]
			}
			return strings.SplitN(parts[1], "/", 2)[0]
		}
	}
	return "(unknown)"
}
