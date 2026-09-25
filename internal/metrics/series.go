package metrics

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Series is one resource over the window, in fixed buckets. Each value is the
// peak inside its bucket, not the mean: a mean over hours erases exactly the
// spike that sets a limit. A bucket with no sample is NaN, never 0, for the
// same reason a missing signal is never a zero anywhere else in this package.
type Series struct {
	Start  time.Time
	Step   time.Duration
	Values []float64
}

// Usage is the CPU (cores) and memory working set (bytes) of one container
// across the given pods, as the worst pod in each bucket. Per pod because a
// request is per pod; the worst one because that is the pod a smaller request
// would starve.
//
// Only the listed pods are queried, which is the set the report analysed. A
// rollout inside the window therefore shows as a series that starts late,
// matching the numbers beside it rather than the workload's full history.
func (p *Prom) Usage(ctx context.Context, namespace string, pods []string, container string, window time.Duration, buckets int) (cpu, mem Series, err error) {
	if len(pods) == 0 {
		return cpu, mem, fmt.Errorf("no pods to query")
	}
	quoted := make([]string, len(pods))
	for i, pod := range pods {
		quoted[i] = regexp.QuoteMeta(pod)
	}
	sel := fmt.Sprintf(`namespace=%q,pod=~%q,container=%q`, namespace, strings.Join(quoted, "|"), container)

	// Whole minutes, because durationString truncates to them and the subquery
	// range must equal the step or buckets overlap or leave gaps.
	step := (window / time.Duration(buckets)).Truncate(time.Minute)
	if step < time.Minute {
		step = time.Minute
	}
	s := durationString(step)
	end := time.Now().Truncate(step)
	start := end.Add(-step * time.Duration(buckets-1))

	// The same 1m resolution the report's CPU peak uses, so the tallest bar
	// and the PEAK column are the same measurement.
	cpu, err = p.queryRange(ctx, fmt.Sprintf(`max(max_over_time(rate(container_cpu_usage_seconds_total{%s}[1m])[%s:1m]))`, sel, s), start, step, buckets)
	if err != nil {
		return cpu, mem, fmt.Errorf("cpu: %w", err)
	}
	mem, err = p.queryRange(ctx, fmt.Sprintf(`max(max_over_time(container_memory_working_set_bytes{%s}[%s]))`, sel, s), start, step, buckets)
	if err != nil {
		return cpu, mem, fmt.Errorf("memory: %w", err)
	}
	return cpu, mem, nil
}

func (p *Prom) queryRange(ctx context.Context, expr string, start time.Time, step time.Duration, buckets int) (Series, error) {
	out := Series{Start: start, Step: step, Values: make([]float64, buckets)}
	for i := range out.Values {
		out.Values[i] = math.NaN()
	}
	end := start.Add(step * time.Duration(buckets-1))
	body, err := p.transport.Get(ctx, "/api/v1/query_range", url.Values{
		"query": {expr},
		"start": {strconv.FormatInt(start.Unix(), 10)},
		"end":   {strconv.FormatInt(end.Unix(), 10)},
		"step":  {strconv.FormatInt(int64(step/time.Second), 10)},
	})
	if err != nil {
		return out, err
	}
	var pr struct {
		Status string `json:"status"`
		Error  string `json:"error"`
		Data   struct {
			Result []struct {
				Values [][2]any `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &pr); err != nil {
		return out, fmt.Errorf("decoding response: %w", err)
	}
	if pr.Status != "success" {
		return out, fmt.Errorf("query failed: %s", pr.Error)
	}
	// max() leaves at most one series. Points are placed by timestamp, not by
	// position, because Prometheus omits a step with no data rather than
	// returning a null for it.
	for _, r := range pr.Data.Result {
		for _, pt := range r.Values {
			ts, ok1 := pt[0].(float64)
			vs, ok2 := pt[1].(string)
			if !ok1 || !ok2 {
				continue
			}
			v, err := strconv.ParseFloat(vs, 64)
			if err != nil || v != v {
				continue
			}
			i := int(math.Round((ts - float64(start.Unix())) / step.Seconds()))
			if i >= 0 && i < buckets {
				out.Values[i] = v
			}
		}
	}
	return out, nil
}
