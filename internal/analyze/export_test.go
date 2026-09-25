package analyze

import "time"

// IsIdleForTest exposes the idle guard so the coverage rule can be tested
// directly. Driving it through a full simulation would mean simulating a
// 90-minute-old container inside a two-week window, which the simulator cannot
// express, and the guard is precisely the part most worth testing.
func IsIdleForTest(f *Finding, window time.Duration, th Thresholds) string {
	r := isIdle(f, window, th)
	if r == nil {
		return ""
	}
	return r.Text
}
