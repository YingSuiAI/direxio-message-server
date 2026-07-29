package task

import (
	"crypto/sha256"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

type Schedule struct {
	ID               string       `json:"id"`
	Name             string       `json:"name"`
	Spec             TaskTemplate `json:"spec"`
	RunAt            *time.Time   `json:"run_at,omitempty"`
	Cron             string       `json:"cron,omitempty"`
	Timezone         string       `json:"timezone"`
	Paused           bool         `json:"paused"`
	Revision         uint64       `json:"revision"`
	NextRunAt        time.Time    `json:"next_run_at,omitempty"`
	LastScheduledFor time.Time    `json:"last_scheduled_for,omitempty"`
	CreatedAt        time.Time    `json:"created_at"`
	UpdatedAt        time.Time    `json:"updated_at"`
	Deleted          bool         `json:"deleted,omitempty"`
}

var cronToken = regexp.MustCompile(`^[0-9*/\-,]+$`)

type CronExpression struct{ Fields [5]string }

func ParseCron(expr string) (CronExpression, error) {
	var parsed CronExpression
	fields := strings.Fields(strings.TrimSpace(expr))
	if len(fields) != 5 {
		return parsed, ErrInvalid
	}
	for i, field := range fields {
		if !cronToken.MatchString(field) || !validCronField(field, i) {
			return CronExpression{}, ErrInvalid
		}
		parsed.Fields[i] = normalizeCronField(field, i)
	}
	return parsed, nil
}

func validCronField(field string, position int) bool {
	if field == "*" {
		return true
	}
	for _, item := range strings.Split(field, ",") {
		if item == "" {
			return false
		}
		if strings.Contains(item, "/") {
			if strings.Count(item, "/") != 1 {
				return false
			}
			pair := strings.SplitN(item, "/", 2)
			item = pair[0]
			if !validCronNumber(pair[1], 1, cronMax[position]-cronMin[position]) {
				return false
			}
		}
		if item == "*" {
			continue
		}
		if strings.Count(item, "-") > 1 {
			return false
		}
		if strings.Contains(item, "-") {
			pair := strings.SplitN(item, "-", 2)
			if !validCronNumber(pair[0], cronMin[position], cronMax[position]) || !validCronNumber(pair[1], cronMin[position], cronMax[position]) {
				return false
			}
			n1, _ := strconv.Atoi(pair[0])
			n2, _ := strconv.Atoi(pair[1])
			if n1 > n2 {
				return false
			}
			continue
		}
		if !validCronNumber(item, cronMin[position], cronMax[position]) {
			return false
		}
	}
	return true
}

var cronMin = [...]int{0, 0, 1, 1, 0}
var cronMax = [...]int{59, 23, 31, 12, 7}

func validCronNumber(value string, min, max int) bool {
	n, err := strconv.Atoi(value)
	return err == nil && n >= min && n <= max
}

func normalizeCronField(field string, position int) string {
	_ = position
	return field
}

func (s Schedule) Normalize() (Schedule, error) {
	s.Name = strings.TrimSpace(s.Name)
	if !ValidUUID(s.ID) || s.Name == "" || !utf8.ValidString(s.Name) || len([]byte(s.Name)) > 512 || s.Revision == 0 || s.CreatedAt.IsZero() || s.UpdatedAt.IsZero() || s.CreatedAt.Location() != time.UTC || s.UpdatedAt.Location() != time.UTC {
		return Schedule{}, ErrInvalid
	}
	var err error
	if s.Spec, err = s.Spec.Normalize(); err != nil {
		return Schedule{}, err
	}
	if (s.RunAt == nil) == (strings.TrimSpace(s.Cron) == "") {
		return Schedule{}, ErrInvalid
	}
	if s.RunAt != nil {
		if s.RunAt.IsZero() {
			return Schedule{}, ErrInvalid
		}
		runAt := s.RunAt.UTC()
		s.RunAt = &runAt
	} else if err := ValidateCron(s.Cron); err != nil {
		return Schedule{}, err
	}
	if s.RunAt == nil {
		s.Cron = strings.Join(strings.Fields(s.Cron), " ")
	}
	s.Timezone = strings.TrimSpace(s.Timezone)
	if s.Timezone == "" {
		s.Timezone = "UTC"
	}
	loc, err := time.LoadLocation(s.Timezone)
	if err != nil {
		return Schedule{}, fmt.Errorf("%w: timezone", ErrInvalid)
	}
	_ = loc
	if !s.NextRunAt.IsZero() {
		s.NextRunAt = s.NextRunAt.UTC()
	}
	if !s.LastScheduledFor.IsZero() {
		s.LastScheduledFor = s.LastScheduledFor.UTC()
	}
	return s, nil
}

func (s Schedule) Validate() error {
	_, err := s.Normalize()
	return err
}

func ValidateCron(expr string) error {
	_, err := ParseCron(expr)
	return err
}

type CronCalculator interface {
	Next(after time.Time, expression, timezone string) (time.Time, error)
}

// NextCron is the dependency-free cursor calculator used when a schedule is
// created or changed. Runtime may supply its own CronCalculator for polling.
func NextCron(after time.Time, expression, timezone string) (time.Time, error) {
	p, err := ParseCron(expression)
	if err != nil {
		return time.Time{}, err
	}
	loc, err := time.LoadLocation(strings.TrimSpace(timezone))
	if err != nil {
		return time.Time{}, err
	}
	for n := 1; n <= 60*24*366*10; n++ {
		v := after.UTC().Truncate(time.Minute).Add(time.Duration(n) * time.Minute).In(loc)
		if cronFieldMatches(p.Fields[0], v.Minute(), 0, 59) && cronFieldMatches(p.Fields[1], v.Hour(), 0, 23) && cronFieldMatches(p.Fields[3], int(v.Month()), 1, 12) {
			dom, dow := cronFieldMatches(p.Fields[2], v.Day(), 1, 31), cronFieldMatches(p.Fields[4], int(v.Weekday()), 0, 7)
			if (p.Fields[2] == "*" && p.Fields[4] == "*") || (p.Fields[2] == "*" && dow) || (p.Fields[4] == "*" && dom) || (p.Fields[2] != "*" && p.Fields[4] != "*" && (dom || dow)) {
				return v.UTC(), nil
			}
		}
	}
	return time.Time{}, ErrInvalid
}
func cronFieldMatches(field string, value, min, max int) bool {
	if value == 0 && max == 7 { // cron permits Sunday as 7.
		if cronFieldMatches(field, 7, min, max) {
			return true
		}
	}
	for _, item := range strings.Split(field, ",") {
		step, base := 1, item
		if strings.Contains(item, "/") {
			parts := strings.SplitN(item, "/", 2)
			base = parts[0]
			step, _ = strconv.Atoi(parts[1])
		}
		lo, hi := min, max
		if base != "*" {
			if strings.Contains(base, "-") {
				p := strings.SplitN(base, "-", 2)
				lo, _ = strconv.Atoi(p[0])
				hi, _ = strconv.Atoi(p[1])
			} else {
				lo, _ = strconv.Atoi(base)
				hi = lo
			}
		}
		if value >= lo && value <= hi && (value-lo)%step == 0 {
			return true
		}
	}
	return false
}

type Occurrence struct {
	ID           string    `json:"id"`
	ScheduleID   string    `json:"schedule_id"`
	ScheduledFor time.Time `json:"scheduled_for"`
	TriggerKey   string    `json:"trigger_key,omitempty"`
	TaskID       string    `json:"task_id"`
	CreatedAt    time.Time `json:"created_at"`
}

func (o Occurrence) Validate() error {
	if !ValidUUID(o.ID) || !ValidUUID(o.ScheduleID) || !ValidUUID(o.TaskID) || o.ScheduledFor.IsZero() || o.CreatedAt.IsZero() {
		return ErrInvalid
	}
	if o.ScheduledFor.Location() != time.UTC || o.CreatedAt.Location() != time.UTC {
		return ErrInvalid
	}
	if o.TriggerKey != "" && !ValidUUID(o.TriggerKey) {
		return ErrInvalid
	}
	return nil
}

// TriggerNowCommand contains immutable idempotency identity and first-create input.
// Once an occurrence exists, its persisted values win over later command input.
type TriggerNowCommand struct {
	ScheduleID, IdempotencyKey string
	At                         time.Time
}

// The durable schedule boundary carries the same replay identity as task
// mutations.  Keeping it in the domain contract prevents a transport adapter
// from accidentally performing a non-idempotent schedule transition.
type CreateScheduleCommand struct {
	Schedule Schedule
	Mutation MutationCommand
}

func (c CreateScheduleCommand) Validate() error {
	if c.Mutation.Validate() != nil || c.Mutation.ExpectedRevision != 0 {
		return ErrInvalid
	}
	return c.Schedule.Validate()
}

type UpdateScheduleCommand struct {
	Schedule Schedule
	Mutation MutationCommand
}

func (c UpdateScheduleCommand) Validate() error {
	if c.Mutation.ValidateExpectedRevision() != nil || c.Schedule.Validate() != nil {
		return ErrInvalid
	}
	return nil
}

type ScheduleMutationCommand struct {
	ScheduleID string
	Mutation   MutationCommand
	At         time.Time
}

func (c ScheduleMutationCommand) Validate() error {
	if !ValidUUID(c.ScheduleID) || c.Mutation.ValidateExpectedRevision() != nil || c.At.IsZero() {
		return ErrInvalid
	}
	return nil
}

type TriggerScheduleCommand struct {
	ScheduleID string
	Mutation   MutationCommand
	At         time.Time
}

func (c TriggerScheduleCommand) Validate() error {
	if !ValidUUID(c.ScheduleID) || c.Mutation.Validate() != nil || c.Mutation.ExpectedRevision != 0 || c.At.IsZero() {
		return ErrInvalid
	}
	return nil
}

type TriggerNowRequest = TriggerNowCommand

func TriggerNow(s Schedule, request TriggerNowRequest) (Occurrence, error) {
	if err := s.Validate(); err != nil {
		return Occurrence{}, err
	}
	if s.Deleted || request.ScheduleID != s.ID || !ValidUUID(request.IdempotencyKey) || request.At.IsZero() {
		return Occurrence{}, ErrInvalid
	}
	request.At = request.At.UTC()
	trigger := request.IdempotencyKey
	occID := deterministicUUID(s.ID + ":trigger:" + trigger)
	taskID := deterministicUUID(occID + ":task")
	return Occurrence{ID: occID, ScheduleID: s.ID, ScheduledFor: request.At.UTC(), TriggerKey: trigger, TaskID: taskID, CreatedAt: request.At.UTC()}, nil
}

func MaterializeOccurrence(s Schedule, occurrence Occurrence) (TaskSpec, error) {
	if err := s.Validate(); err != nil {
		return TaskSpec{}, err
	}
	if err := occurrence.Validate(); err != nil || occurrence.ScheduleID != s.ID || s.Deleted {
		return TaskSpec{}, ErrInvalid
	}
	key := deterministicUUID(occurrence.ID + ":idempotency")
	spec, err := s.Spec.Materialize(key, occurrence.ScheduledFor)
	if err != nil {
		return TaskSpec{}, err
	}
	return spec, nil
}

func deterministicUUID(seed string) string { return uuid.UUID(uuidV5Namespace(seed)).String() }

// uuidV5Namespace avoids exposing a mutable process-global namespace.
func uuidV5Namespace(seed string) [16]byte {
	var out [16]byte
	h := sha256Bytes([]byte(seed))
	copy(out[:], h[:16])
	out[6] = (out[6] & 0x0f) | 0x50
	out[8] = (out[8] & 0x3f) | 0x80
	return out
}
func sha256Bytes(b []byte) [32]byte { return sha256.Sum256(b) }
