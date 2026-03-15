package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

// batchSeparator is a marker used to join/split multiple segments in a single
// Google Translate request. We use a visible token that GT will pass through
// unchanged. The surrounding newlines force GT to treat each segment independently.
const batchSeparator = "\n|||SEG|||\n"

// maxQueryLen caps the URL-encoded query parameter size. Google's free endpoint
// rejects requests with q > ~5000 chars, so we split into multiple batches
// if needed. Using POST with a body would raise the limit, but the free
// endpoint doesn't reliably support it.
const maxQueryLen = 4500

// translateBatch translates a slice of texts in as few HTTP round-trips as
// possible by joining them with a separator, sending to Google Translate, and
// splitting the result back. Returns a same-length slice of translated strings.
func translateBatch(texts []string) ([]string, error) {
	if len(texts) == 0 {
		return nil, nil
	}

	// Build batches that fit within maxQueryLen
	type batch struct {
		startIdx int
		segments []string
	}
	var batches []batch
	cur := batch{startIdx: 0}
	curLen := 0

	for i, t := range texts {
		segLen := len(t)
		if len(cur.segments) > 0 {
			segLen += len(batchSeparator)
		}
		if curLen+segLen > maxQueryLen && len(cur.segments) > 0 {
			batches = append(batches, cur)
			cur = batch{startIdx: i}
			curLen = 0
		}
		cur.segments = append(cur.segments, t)
		curLen += segLen
	}
	if len(cur.segments) > 0 {
		batches = append(batches, cur)
	}

	// Cap concurrency to avoid hammering Google and getting rate-limited
	const maxConcurrent = 10

	log.Printf("[TRANSLATE] %d segments -> %d API call(s) (concurrency: %d)", len(texts), len(batches), min(maxConcurrent, len(batches)))

	results := make([]string, len(texts))
	// Pre-fill with originals as fallback
	copy(results, texts)

	var wg sync.WaitGroup
	sem := make(chan struct{}, maxConcurrent)

	for batchIdx, b := range batches {
		wg.Add(1)
		go func(idx int, b batch) {
			defer wg.Done()
			sem <- struct{}{}        // acquire
			defer func() { <-sem }() // release

			joined := strings.Join(b.segments, batchSeparator)

			translated, err := callTranslateAPI(joined)
			if err != nil {
				log.Printf("[TRANSLATE] Batch %d/%d FAILED: %v", idx+1, len(batches), err)
				return // results already has originals
			}

			log.Printf("[TRANSLATE] Batch %d/%d OK", idx+1, len(batches))

			// Split the translated text back into segments
			parts := strings.Split(translated, batchSeparator)
			if len(parts) != len(b.segments) {
				parts = fuzzySlitBySeparator(translated, len(b.segments))
			}

			for i := range b.segments {
				if i < len(parts) {
					results[b.startIdx+i] = strings.TrimSpace(parts[i])
				}
			}
		}(batchIdx, b)
	}

	wg.Wait()

	// Log translations
	translated := 0
	for i, orig := range texts {
		if results[i] != orig {
			translated++
			log.Printf("[TRANSLATE] OK: %q -> %q", truncate(orig, 60), truncate(results[i], 60))
		}
	}
	log.Printf("[TRANSLATE] %d/%d segments translated", translated, len(texts))

	return results, nil
}

// fuzzySlitBySeparator attempts to split translated text when the exact
// separator wasn't preserved. It looks for variations of the separator marker.
func fuzzySlitBySeparator(text string, expectedParts int) []string {
	// Try common mangled forms
	for _, sep := range []string{
		batchSeparator,
		"\n||| SEG |||\n",
		"\n|||seg|||\n",
		"\n||| seg |||\n",
		"\n|||SEG ||\n",
		"|||SEG|||",
		"||| SEG |||",
		"|||seg|||",
		"||| seg |||",
	} {
		parts := strings.Split(text, sep)
		if len(parts) == expectedParts {
			return parts
		}
	}
	// Last resort: split on any line that looks like it's mostly pipes
	lines := strings.Split(text, "\n")
	var parts []string
	var cur strings.Builder
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.Contains(trimmed, "|||") || strings.Contains(trimmed, "SEG") || strings.Contains(trimmed, "seg") {
			if cur.Len() > 0 {
				parts = append(parts, cur.String())
				cur.Reset()
			}
			continue
		}
		if cur.Len() > 0 {
			cur.WriteString("\n")
		}
		cur.WriteString(line)
	}
	if cur.Len() > 0 {
		parts = append(parts, cur.String())
	}
	if len(parts) == expectedParts {
		return parts
	}
	// Give up: return the whole text as a single part
	return []string{text}
}

// callTranslateAPI sends a single translation request to Google Translate.
func callTranslateAPI(text string) (string, error) {
	if strings.TrimSpace(text) == "" {
		return text, nil
	}

	apiURL := "https://translate.googleapis.com/translate_a/single"

	params := url.Values{}
	params.Set("client", "gtx")
	params.Set("sl", "auto")
	params.Set("tl", "en")
	params.Set("dt", "t")
	params.Set("q", text)

	resp, err := http.Get(apiURL + "?" + params.Encode())
	if err != nil {
		return "", fmt.Errorf("translate request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read translate response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("translate API returned %d: %s", resp.StatusCode, string(body))
	}

	var result []interface{}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("parse translate response: %w", err)
	}

	var translated strings.Builder
	if sentences, ok := result[0].([]interface{}); ok {
		for _, s := range sentences {
			if parts, ok := s.([]interface{}); ok && len(parts) > 0 {
				if t, ok := parts[0].(string); ok {
					translated.WriteString(t)
				}
			}
		}
	}

	out := translated.String()
	if out == "" {
		return text, nil
	}

	return out, nil
}
