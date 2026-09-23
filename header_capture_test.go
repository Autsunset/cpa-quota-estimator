package main

import (
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCodexHeaderCaptureIsOptIn(t *testing.T) {
	s, err := openStore(filepath.Join(t.TempDir(), "headers.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	a := app{cfg: defaultConfig(), store: s}
	r := usageRecord{AuthID: "a", Model: "gpt-5.6-sol", RequestedAt: time.Unix(100, 0),
		Detail: usageDetail{InputTokens: 10, TotalTokens: 10},
		ResponseHeaders: http.Header{
			"X-Codex-Primary-Used-Percent": {"0"},
			"X-Codex-Extra":                {"one", "two"},
			"Authorization":                {"must-not-store"},
		},
	}
	if err = a.recordUsage(r); err != nil {
		t.Fatal(err)
	}
	a.cfg.CaptureCodexHeaders = true
	r.RequestedAt = time.Unix(110, 0)
	if err = a.recordUsage(r); err != nil {
		t.Fatal(err)
	}
	rows, err := s.db.Query(`SELECT codex_headers_json FROM usage_events ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var values []string
	for rows.Next() {
		var value string
		if err = rows.Scan(&value); err != nil {
			t.Fatal(err)
		}
		values = append(values, value)
	}
	if len(values) != 2 || values[0] != "" || !strings.Contains(values[1], `"X-Codex-Extra":["one","two"]`) || strings.Contains(values[1], "must-not-store") {
		t.Fatalf("captured headers = %#v", values)
	}
}
