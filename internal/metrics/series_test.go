package metrics

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Usage asks for exactly the analysed pods of one container, and places points
// by timestamp: Prometheus omits an empty step rather than sending a null, so
// placing by position would shift every later point left and close the gap.
func TestUsagePlacesPointsByTime(t *testing.T) {
	var exprs []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/query_range" {
			t.Errorf("path = %s", r.URL.Path)
		}
		q := r.URL.Query()
		exprs = append(exprs, q.Get("query"))
		start, _ := strconv.ParseInt(q.Get("start"), 10, 64)
		step, _ := strconv.ParseInt(q.Get("step"), 10, 64)
		// Points 0, 1 and 3: step 2 has no data.
		var vals [][2]any
		for _, i := range []int64{0, 1, 3} {
			vals = append(vals, [2]any{start + i*step, strconv.Itoa(int(i) + 1)})
		}
		b, _ := json.Marshal(map[string]any{"status": "success", "data": map[string]any{
			"resultType": "matrix", "result": []any{map[string]any{"metric": map[string]string{}, "values": vals}},
		}})
		w.Write(b)
	}))
	defer srv.Close()

	cpu, mem, err := NewProm(srv.URL).Usage(context.Background(), "prod", []string{"web-a", "web-b"}, "app", 4*time.Hour, 4)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []Series{cpu, mem} {
		if s.Values[0] != 1 || s.Values[1] != 2 || !math.IsNaN(s.Values[2]) || s.Values[3] != 4 {
			t.Errorf("values = %v, want [1 2 NaN 4]", s.Values)
		}
	}
	for _, e := range exprs {
		if !strings.Contains(e, `namespace="prod",pod=~"web-a|web-b",container="app"`) {
			t.Errorf("query does not select the analysed pods: %s", e)
		}
	}
}
