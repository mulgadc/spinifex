package ecs

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestTaskToAWS_LifecycleTimestamps(t *testing.T) {
	svc, _ := newTestService(t)
	created := time.Date(2026, 10, 29, 1, 0, 0, 0, time.UTC)
	started := created.Add(30 * time.Second)
	stopped := started.Add(time.Hour)

	cases := []struct {
		name             string
		rec              TaskRecord
		started, stopped *time.Time
	}{
		{"pending", TaskRecord{LastStatus: TaskStatusPending, CreatedAt: created}, nil, nil},
		{"running", TaskRecord{LastStatus: TaskStatusRunning, CreatedAt: created, StartedAt: started}, &started, nil},
		{"stopped", TaskRecord{LastStatus: TaskStatusStopped, CreatedAt: created, StartedAt: started, StoppedAt: stopped}, &started, &stopped},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			task := svc.taskToAWS(testAccountID, &tc.rec)
			assert.Equal(t, &created, task.CreatedAt)
			assert.Equal(t, tc.started, task.StartedAt)
			assert.Equal(t, tc.stopped, task.StoppedAt)
		})
	}
}
