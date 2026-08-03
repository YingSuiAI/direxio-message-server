package schedules

import (
	"fmt"
	"time"

	cron "github.com/robfig/cron/v3"
)

// nextCron computes the next occurrence for standard five-field cron. It
// intentionally coalesces downtime by searching strictly after now.
func nextCron(expr, tz string, now time.Time) (*time.Time, error) {
	if tz == "" {
		return nil, fmt.Errorf("cron timezone is required")
	}
	loc, e := time.LoadLocation(tz)
	if e != nil {
		return nil, e
	}
	parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)
	sched, err := parser.Parse(expr)
	if err != nil {
		return nil, fmt.Errorf("invalid cron: %w", err)
	}
	// cron.Schedule.Next is strictly after the reference instant. Evaluate in
	// the requested IANA location and persist the resulting instant as UTC.
	t := sched.Next(now.In(loc))
	if t.IsZero() {
		return nil, fmt.Errorf("cron has no next occurrence")
	}
	u := t.UTC()
	return &u, nil
}
