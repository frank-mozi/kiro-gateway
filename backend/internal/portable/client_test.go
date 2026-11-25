package portable

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestDrainWaitsAndRejectsNewRequests(t *testing.T) {
	c := &Client{token: "test"}
	started, finish := make(chan struct{}), make(chan struct{})
	handler := c.Guard(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-finish
		w.WriteHeader(200)
	}))
	var work sync.WaitGroup
	work.Add(1)
	go func() {
		defer work.Done()
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/v1/responses", nil))
	}()
	<-started
	req := httptest.NewRequest("POST", "/_portable/drain", nil)
	req.Header.Set("Authorization", "Bearer test")
	done := make(chan struct{})
	response := httptest.NewRecorder()
	go func() { c.ControlHandler("0.3.0").ServeHTTP(response, req); close(done) }()
	deadline := time.Now().Add(time.Second)
	for {
		c.mu.Lock()
		draining := c.draining
		c.mu.Unlock()
		if draining {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("drain did not start")
		}
		time.Sleep(time.Millisecond)
	}
	blocked := httptest.NewRecorder()
	handler.ServeHTTP(blocked, httptest.NewRequest("POST", "/v1/messages", nil))
	if blocked.Code != 503 {
		t.Fatal(blocked.Code)
	}
	select {
	case <-done:
		t.Fatal("drain finished before in-flight work")
	default:
	}
	close(finish)
	work.Wait()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("drain did not finish")
	}
	if response.Code != 200 {
		t.Fatal(response.Code)
	}
}

func TestPortableControlRequiresToken(t *testing.T) {
	c := &Client{token: "secret", instance: "instance"}
	w := httptest.NewRecorder()
	c.ControlHandler("0.3.0").ServeHTTP(w, httptest.NewRequest("GET", "/_portable/health", nil))
	if w.Code != 401 {
		t.Fatal(w.Code)
	}
	m := &Manager{ctx: context.Background(), token: "secret"}
	w = httptest.NewRecorder()
	m.ServeHTTP(w, httptest.NewRequest("POST", "/install", strings.NewReader(`{"version":"9.9.9"}`)))
	if w.Code != 401 {
		t.Fatal(w.Code)
	}
}
