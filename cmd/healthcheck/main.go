package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

const defaultHealthURL = "http://127.0.0.1:8317/"

func check(client *http.Client, rawURL string) error {
	if client == nil {
		return fmt.Errorf("health client is unavailable")
	}
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		rawURL = defaultHealthURL
	}
	request, errRequest := http.NewRequest(http.MethodGet, rawURL, nil)
	if errRequest != nil {
		return fmt.Errorf("build health request: %w", errRequest)
	}
	response, errDo := client.Do(request)
	if errDo != nil {
		return fmt.Errorf("health request failed: %w", errDo)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("health endpoint returned HTTP %d", response.StatusCode)
	}
	return nil
}

func main() {
	client := &http.Client{Timeout: 3 * time.Second}
	if errCheck := check(client, os.Getenv("CLIPROXY_HEALTHCHECK_URL")); errCheck != nil {
		os.Exit(1)
	}
}
