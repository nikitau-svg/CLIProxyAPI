package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestCheckAcceptsSuccessfulEndpoint(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write([]byte("ok"))
	}))
	defer server.Close()

	if errCheck := check(&http.Client{Timeout: time.Second}, server.URL); errCheck != nil {
		t.Fatalf("check() error = %v", errCheck)
	}
}

func TestCheckRejectsNonSuccessfulEndpoint(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		http.Error(writer, "not ready", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	if errCheck := check(&http.Client{Timeout: time.Second}, server.URL); errCheck == nil {
		t.Fatal("check() accepted HTTP 503")
	}
}
