package storage

import (
	"context"
	"encoding/json"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestDatabaseScheduleListsEncodeEmptyArrays(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := NewUnmigratedDatabaseStore(db, nil)

	mock.ExpectQuery(regexp.QuoteMeta("SELECT schedule_id,owner_id,name,prompt,trigger_kind,trigger_value,timezone,skip_if_running,status,core_state,revision,model_profile_id,model_profile_revision,credential_version,next_run_at,latest_run_at,lease_owner,lease_until,lease_epoch,task_template,trigger_json,created_at,updated_at FROM p2p_agent_schedules")).
		WillReturnRows(sqlmock.NewRows([]string{"schedule_id"}))
	schedules, err := store.ListSchedules(context.Background(), "owner", 50, "")
	if err != nil {
		t.Fatal(err)
	}
	if schedules.Schedules == nil {
		t.Fatal("empty schedule list must be a non-nil slice")
	}
	scheduleJSON, err := json.Marshal(map[string]any{"schedules": schedules.Schedules, "next_cursor": schedules.NextCursor})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(scheduleJSON), `{"next_cursor":"","schedules":[]}`; got != want {
		t.Fatalf("schedule list JSON = %s, want %s", got, want)
	}

	mock.ExpectQuery(regexp.QuoteMeta("SELECT run_id,schedule_id,owner_id,status,scheduled_for,started_at,finished_at,result,error,lease_epoch FROM p2p_agent_schedule_runs")).
		WillReturnRows(sqlmock.NewRows([]string{"run_id"}))
	runs, err := store.ListScheduleRuns(context.Background(), "owner", "schedule", 50, "")
	if err != nil {
		t.Fatal(err)
	}
	if runs.Runs == nil {
		t.Fatal("empty schedule run list must be a non-nil slice")
	}
	runsJSON, err := json.Marshal(map[string]any{"runs": runs.Runs, "next_cursor": runs.NextCursor})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(runsJSON), `{"next_cursor":"","runs":[]}`; got != want {
		t.Fatalf("schedule run list JSON = %s, want %s", got, want)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
