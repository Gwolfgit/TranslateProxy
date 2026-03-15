package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
)

// translateText translates text from auto-detected language to English
// using the free Google Translate API endpoint.
func translateText(text string) (string, error) {
	if strings.TrimSpace(text) == "" {
		return text, nil
	}

	// Google Translate free endpoint
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

	// Response is a nested JSON array: [[["translated","original",...],...],...]
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
		return text, nil // Fallback to original if parsing fails
	}

	log.Printf("Translated: %q -> %q", text, out)
	return out, nil
}
