package main

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"fmt"
	"io"
	"log"
	"net/http"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// Global tiered cache: 100k in-memory LRU + BoltDB on disk, 24h TTL.
var cache *tieredCache

func main() {
	log.Println("TranslateProxy ICAP translator starting on :1344")

	var err error
	cache, err = newTieredCache(100_000, "/data/translations.db", 24*time.Hour)
	if err != nil {
		log.Fatalf("Failed to initialize cache: %v", err)
	}
	defer cache.Close()

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

// readBody reads and decompresses the HTTP response body (gzip, deflate, br).
func readBody(resp *http.Response) ([]byte, error) {
	if resp.Body == nil {
		return nil, nil
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading body: %w", err)
	}

	encoding := strings.ToLower(resp.Header.Get("Content-Encoding"))
	log.Printf("[ICAP]   Content-Encoding: %q, raw size: %d bytes", encoding, len(raw))

	switch encoding {
	case "gzip":
		gz, err := gzip.NewReader(bytes.NewReader(raw))
		if err != nil {
			return nil, fmt.Errorf("gzip reader: %w", err)
		}
		defer gz.Close()
		return io.ReadAll(gz)
	case "deflate":
		return io.ReadAll(flate.NewReader(bytes.NewReader(raw)))
	case "br":
		return decompressBrotli(raw)
	}

	return raw, nil
}

// decompressBrotli uses the brotli command-line tool for decompression.
func decompressBrotli(data []byte) ([]byte, error) {
	cmd := exec.Command("brotli", "-d")
	cmd.Stdin = bytes.NewReader(data)
	var out bytes.Buffer
	cmd.Stdout = &out
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("brotli decompress: %w: %s", err, stderr.String())
	}
	return out.Bytes(), nil
}

// cyrillicRunRegex matches runs of 2+ Cyrillic characters (with spaces/punctuation between).
// Single Cyrillic chars are ignored — they're typically encoding artifacts, not real text.
var cyrillicRunRegex = regexp.MustCompile(`[\p{Cyrillic}][\p{Cyrillic}\s\p{P}\p{N}]*[\p{Cyrillic}]`)

// translateContentWithStats collects all Cyrillic runs, batch-translates them
// in minimal API calls, then replaces them in the original body.
func translateContentWithStats(body string) (string, int, error) {
	// Find all matches with their positions
	matches := cyrillicRunRegex.FindAllStringIndex(string(body), -1)
	if len(matches) == 0 {
		return body, 0, nil
	}

	// Deduplicate: collect unique strings to translate, preserving order
	type matchInfo struct {
		start, end int
		text       string
	}
	var allMatches []matchInfo
	uniqueTexts := make(map[string]int) // text -> index in uniqueList
	var uniqueList []string

	for _, loc := range matches {
		text := body[loc[0]:loc[1]]
		if _, exists := uniqueTexts[text]; !exists {
			uniqueTexts[text] = len(uniqueList)
			uniqueList = append(uniqueList, text)
		}
		allMatches = append(allMatches, matchInfo{start: loc[0], end: loc[1], text: text})
	}

	log.Printf("[TRANSLATE] %d total matches, %d unique segments to translate", len(allMatches), len(uniqueList))

	translateStart := time.Now()
	translated, err := translateBatch(uniqueList)
	translateElapsed := time.Since(translateStart)

	if err != nil {
		log.Printf("[TRANSLATE] Batch translation failed (%v): %v", translateElapsed, err)
		return body, 0, nil
	}

	log.Printf("[TRANSLATE] Batch translation completed in %v", translateElapsed)

	// Build the translation lookup
	translationMap := make(map[string]string, len(uniqueList))
	numTranslated := 0
	for i, orig := range uniqueList {
		if i < len(translated) && translated[i] != orig {
			translationMap[orig] = translated[i]
			numTranslated++
		} else {
			translationMap[orig] = orig
		}
	}

	// Rebuild body by replacing matches back-to-front (so indices stay valid)
	result := []byte(body)
	for i := len(allMatches) - 1; i >= 0; i-- {
		m := allMatches[i]
		if replacement, ok := translationMap[m.text]; ok {
			result = append(result[:m.start], append([]byte(replacement), result[m.end:]...)...)
		}
	}

	return string(result), numTranslated, nil
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
