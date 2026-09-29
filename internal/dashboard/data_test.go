package dashboard

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
)

func TestConfigReportsLimits(t *testing.T) {
	s := New(nil, nil, nil, Limits{MaxActive: 100, RateRPS: 20, RateBurst: 40})
	req := httptest.NewRequest("GET", "/dashboard/api/config", nil)
	rr := httptest.NewRecorder()
	s.handleGetConfig(rr, req)

	var got map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["max_active"].(float64) != 100 || got["rate_limit_rps"].(float64) != 20 || got["rate_limit_burst"].(float64) != 40 {
		t.Fatalf("config did not report real limits: %v", got)
	}
}
