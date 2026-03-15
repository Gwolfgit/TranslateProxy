package main

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode"
)

func main() {
	log.Println("TranslateProxy ICAP translator starting on :1344")

	server := newICAPServer(":1344", handleTranslation)
	log.Fatal(server.ListenAndServe())
}

// handleTranslation processes ICAP RESPMOD requests.
// Returns nil to signal a 204 (no modification).
func handleTranslation(domain string, httpResp *http.Response) (*http.Response, error) {
	startTime := time.Now()

	if httpResp == nil {
		log.Println("[ICAP] No HTTP response present")
		return nil, nil
	}

	contentType := httpResp.Header.Get("Content-Type")
	contentLength := httpResp.Header.Get("Content-Length")

	log.Printf("[ICAP] === Request received ===")
	log.Printf("[ICAP]   Domain:         %s", domain)
	log.Printf("[ICAP]   Content-Type:   %s", contentType)
	log.Printf("[ICAP]   Content-Length:  %s", contentLength)

	// Only process text-based content
	if !isTextContent(contentType) {
		elapsed := time.Since(startTime)
		log.Printf("[ICAP]   Result:         SKIP (non-text content)")
		log.Printf("[ICAP]   Processing time: %v", elapsed)
		log.Printf("[ICAP] === Request complete ===")
		return nil, nil
	}

	// Read and decompress body
	body, err := readBody(httpResp)
	if err != nil {
		elapsed := time.Since(startTime)
		log.Printf("[ICAP]   Error:          reading body: %v", err)
		log.Printf("[ICAP]   Processing time: %v", elapsed)
		log.Printf("[ICAP] === Request complete ===")
		return nil, nil
	}

	log.Printf("[ICAP]   Body size:      %d bytes (decompressed)", len(body))

	if len(body) == 0 {
		elapsed := time.Since(startTime)
		log.Printf("[ICAP]   Result:         SKIP (empty body)")
		log.Printf("[ICAP]   Processing time: %v", elapsed)
		log.Printf("[ICAP] === Request complete ===")
		return nil, nil
	}

	// Find Cyrillic runs
	cyrillicMatches := cyrillicRunRegex.FindAllString(string(body), -1)
	numDetected := len(cyrillicMatches)

	log.Printf("[ICAP]   Cyrillic strings detected: %d", numDetected)

	if numDetected == 0 {
		elapsed := time.Since(startTime)
		log.Printf("[ICAP]   Result:         PASS-THROUGH (no Cyrillic)")
		log.Printf("[ICAP]   Processing time: %v", elapsed)
		log.Printf("[ICAP] === Request complete ===")
		return nil, nil
	}

	// Log each detected Cyrillic segment (truncated)
	for i, match := range cyrillicMatches {
		display := match
		if len(display) > 80 {
			display = display[:80] + "..."
		}
		log.Printf("[ICAP]   Cyrillic[%d]:    %q", i, display)
	}

	log.Printf("[ICAP]   Action:         TRANSLATING %d segments...", numDetected)

	// Translate
	translated, numTranslated, err := translateContentWithStats(string(body))
	if err != nil {
		elapsed := time.Since(startTime)
		log.Printf("[ICAP]   Error:          translation failed: %v", err)
		log.Printf("[ICAP]   Processing time: %v", elapsed)
		log.Printf("[ICAP] === Request complete ===")
		return nil, nil
	}

	elapsed := time.Since(startTime)
	log.Printf("[ICAP]   Translated:     %d / %d segments", numTranslated, numDetected)
	log.Printf("[ICAP]   Result:         MODIFIED")
	log.Printf("[ICAP]   Processing time: %v", elapsed)
	log.Printf("[ICAP] === Request complete ===")

	// Build modified response
	modResp := &http.Response{
		Status:        httpResp.Status,
		StatusCode:    httpResp.StatusCode,
		Proto:         httpResp.Proto,
		ProtoMajor:    httpResp.ProtoMajor,
		ProtoMinor:    httpResp.ProtoMinor,
		Header:        httpResp.Header.Clone(),
		Body:          io.NopCloser(bytes.NewReader([]byte(translated))),
		ContentLength: int64(len(translated)),
	}

	return modResp, nil
}

// isTextContent returns true if the content type is text-based.
func isTextContent(ct string) bool {
	ct = strings.ToLower(ct)
	textTypes := []string{
		"text/html",
		"text/plain",
		"text/xml",
		"application/json",
		"application/xml",
		"application/xhtml+xml",
		"application/javascript",
		"text/javascript",
	}
	for _, t := range textTypes {
		if strings.Contains(ct, t) {
			return true
		}
	}
	return false
}

// readBody reads and decompresses the HTTP response body.
func readBody(resp *http.Response) ([]byte, error) {
	if resp.Body == nil {
		return nil, nil
	}
	defer resp.Body.Close()

	var reader io.Reader = resp.Body

	encoding := strings.ToLower(resp.Header.Get("Content-Encoding"))
	log.Printf("[ICAP]   Content-Encoding: %s", encoding)

	switch encoding {
	case "gzip":
		gz, err := gzip.NewReader(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("gzip reader: %w", err)
		}
		defer gz.Close()
		reader = gz
	case "deflate":
		reader = flate.NewReader(resp.Body)
	}

	return io.ReadAll(reader)
}

// containsCyrillic checks if the string contains any Cyrillic Unicode characters.
func containsCyrillic(s string) bool {
	for _, r := range s {
		if unicode.Is(unicode.Cyrillic, r) {
			return true
		}
	}
	return false
}

// cyrillicRunRegex matches sequences of Cyrillic characters including spaces/punctuation between them.
var cyrillicRunRegex = regexp.MustCompile(`[\p{Cyrillic}][\p{Cyrillic}\s\p{P}\p{N}]*[\p{Cyrillic}]|[\p{Cyrillic}]+`)

// translateContentWithStats translates all Cyrillic runs and returns stats.
func translateContentWithStats(body string) (string, int, error) {
	numTranslated := 0
	var lastErr error

	result := cyrillicRunRegex.ReplaceAllStringFunc(body, func(match string) string {
		segStart := time.Now()
		translated, err := translateText(match)
		segElapsed := time.Since(segStart)

		if err != nil {
			log.Printf("[TRANSLATE] FAIL (%v): %q -> error: %v", segElapsed, truncate(match, 60), err)
			lastErr = err
			return match
		}

		log.Printf("[TRANSLATE] OK (%v): %q -> %q", segElapsed, truncate(match, 60), truncate(translated, 60))
		numTranslated++
		return translated
	})

	if lastErr != nil {
		log.Printf("[TRANSLATE] Some translations failed: %v", lastErr)
	}

	return result, numTranslated, nil
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
