package ingest

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"briefing-v3/internal/store"
)

// TestOssinsightSource_DefaultPeriod 验证 T3: 默认 period=past_week.
func TestOssinsightSource_DefaultPeriod(t *testing.T) {
	row := &store.Source{
		ID: 1, DomainID: "ai", Type: "ossinsight",
		Name:       "GitHub Trending",
		ConfigJSON: `{"url":"https://api.example.com/trends/repos"}`,
	}
	src, err := newOssInsightSource(row)
	if err != nil {
		t.Fatalf("newOssInsightSource: %v", err)
	}
	osrc := src.(*ossinsightSource)
	if osrc.cfg.Period != "past_week" {
		t.Errorf("default period=past_week expected, got %q", osrc.cfg.Period)
	}
}

// TestOssinsightSource_CustomPeriod 验证 T3: config 可覆盖默认 period.
func TestOssinsightSource_CustomPeriod(t *testing.T) {
	row := &store.Source{
		ID: 2, DomainID: "ai", Type: "ossinsight",
		Name:       "GitHub Trending Daily",
		ConfigJSON: `{"url":"https://api.example.com/trends/repos","period":"past_24_hours"}`,
	}
	src, err := newOssInsightSource(row)
	if err != nil {
		t.Fatalf("newOssInsightSource: %v", err)
	}
	osrc := src.(*ossinsightSource)
	if osrc.cfg.Period != "past_24_hours" {
		t.Errorf("expected past_24_hours, got %q", osrc.cfg.Period)
	}
}

// TestOssinsightSource_FetchAppendsPeriod 验证 T3: Fetch 把 ?period= 拼到 URL.
func TestOssinsightSource_FetchAppendsPeriod(t *testing.T) {
	var capturedURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedURL = r.URL.RequestURI()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"rows":[]}}`))
	}))
	defer srv.Close()

	row := &store.Source{
		ID: 3, DomainID: "ai", Type: "ossinsight",
		Name:       "test",
		ConfigJSON: `{"url":"` + srv.URL + `","period":"past_week"}`,
	}
	src, _ := newOssInsightSource(row)
	_, _ = src.Fetch(context.Background())
	if !strings.Contains(capturedURL, "period=past_week") {
		t.Errorf("expected URL to contain period=past_week, got %q", capturedURL)
	}
}
