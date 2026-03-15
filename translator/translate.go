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
// possible. Cached translations are returned immediately; only cache misses
// are sent to Google Translate (batched and concurrent).
func translateBatch(texts []string) ([]string, error) {
	if len(texts) == 0 {
		return nil, nil
	}

	results := make([]string, len(texts))
	copy(results, texts) // fallback = originals

	// Phase 1: resolve from cache
	type uncachedEntry struct {
		origIdx int    // index in texts/results
		text    string
	}
	var uncached []uncachedEntry
	cacheHits := 0

	for i, t := range texts {
		if translated, ok := cache.Get(t); ok {
			results[i] = translated
			cacheHits++
		} else {
			uncached = append(uncached, uncachedEntry{origIdx: i, text: t})
		}
	}

	log.Printf("[TRANSLATE] %d segments: %d cache hits, %d to translate", len(texts), cacheHits, len(uncached))

	if len(uncached) == 0 {
		cache.LogStats()
		return results, nil
	}

	// Phase 2: batch uncached segments
	type batch struct {
		entries []uncachedEntry
	}
	var batches []batch
	var cur batch
	curLen := 0

	for _, entry := range uncached {
		segLen := len(entry.text)
		if len(cur.entries) > 0 {
			segLen += len(batchSeparator)
		}
		if curLen+segLen > maxQueryLen && len(cur.entries) > 0 {
			batches = append(batches, cur)
			cur = batch{}
			curLen = 0
		}
		cur.entries = append(cur.entries, entry)
		curLen += segLen
	}
	if len(cur.entries) > 0 {
		batches = append(batches, cur)
	}

	const maxConcurrent = 10
	log.Printf("[TRANSLATE] %d uncached -> %d API call(s) (concurrency: %d)", len(uncached), len(batches), min(maxConcurrent, len(batches)))

	// Phase 3: fire batches concurrently
	var wg sync.WaitGroup
	sem := make(chan struct{}, maxConcurrent)

	for batchIdx, b := range batches {
		wg.Add(1)
		go func(idx int, b batch) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			segments := make([]string, len(b.entries))
			for i, e := range b.entries {
				segments[i] = e.text
			}
			joined := strings.Join(segments, batchSeparator)

			translated, err := callTranslateAPI(joined)
			if err != nil {
				log.Printf("[TRANSLATE] Batch %d/%d FAILED: %v", idx+1, len(batches), err)
				return
			}

			log.Printf("[TRANSLATE] Batch %d/%d OK", idx+1, len(batches))

			parts := strings.Split(translated, batchSeparator)
			if len(parts) != len(b.entries) {
				parts = fuzzySlitBySeparator(translated, len(b.entries))
			}

			for i, entry := range b.entries {
				if i < len(parts) {
					result := strings.TrimSpace(parts[i])
					results[entry.origIdx] = result
					// Store in cache
					cache.Put(entry.text, result)
				}
			}
		}(batchIdx, b)
	}

	wg.Wait()

	// Log summary
	translated := 0
	for i, orig := range texts {
		if results[i] != orig {
			translated++
		}
	}
	log.Printf("[TRANSLATE] %d/%d segments translated (%d from cache)", translated, len(texts), cacheHits)
	cache.LogStats()

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
