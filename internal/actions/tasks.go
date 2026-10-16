package actions

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"
)

// deployTask tracks one long-running composite action (currently just
// deploy) so the caller can outlive a transport timeout: every deploy
// registers a task before doing anything, publishes a snapshot after
// each step, and task_status hands those snapshots back (2026-09-25
// report: synchronous deploys routinely outlive the MCP tool timeout and
// the caller was left reverse-engineering progress from jar mtimes).
type deployTask struct {
	ID      string
	Tool    string
	Created time.Time

	mu      sync.Mutex
	updated time.Time
	done    bool
	data    DeployData

	doneCh chan struct{}
}

func (t *deployTask) publish(data DeployData) {
	t.mu.Lock()
	t.data = data
	t.updated = time.Now()
	t.mu.Unlock()
}

func (t *deployTask) finish(data DeployData) {
	t.mu.Lock()
	if !t.done {
		t.done = true
		t.data = data
		t.updated = time.Now()
		close(t.doneCh)
	}
	t.mu.Unlock()
}

// snapshot returns the latest published DeployData, its timestamp and
// whether the task finished. Step result pointers inside DeployData are
// replaced (never mutated) by the worker, so a reader marshalling the
// copy always sees a consistent step set.
func (t *deployTask) snapshot() (DeployData, time.Time, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.data, t.updated, t.done
}

func (t *deployTask) isDone() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.done
}

// taskRegistry keeps the most recent tasks; old finished ones are pruned.
type taskRegistry struct {
	mu    sync.Mutex
	tasks map[string]*deployTask
	seq   int64
}

const maxTrackedTasks = 32

func newTaskRegistry() *taskRegistry {
	return &taskRegistry{tasks: map[string]*deployTask{}}
}

func (tr *taskRegistry) newTask(tool string) *deployTask {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.seq++
	t := &deployTask{
		ID:      fmt.Sprintf("%s-%d-%04d", tool, time.Now().Unix(), tr.seq),
		Tool:    tool,
		Created: time.Now(),
		doneCh:  make(chan struct{}),
	}
	tr.tasks[t.ID] = t
	// Prune oldest finished tasks once over capacity; running tasks are
	// never dropped.
	if len(tr.tasks) > maxTrackedTasks {
		type entry struct {
			id string
			at time.Time
		}
		var finished []entry
		for id, ft := range tr.tasks {
			if ft != t && ft.isDone() {
				finished = append(finished, entry{id, ft.Created})
			}
		}
		sort.Slice(finished, func(i, j int) bool { return finished[i].at.Before(finished[j].at) })
		for i := 0; i < len(finished) && len(tr.tasks) > maxTrackedTasks; i++ {
			delete(tr.tasks, finished[i].id)
		}
	}
	return t
}

func (tr *taskRegistry) get(id string) *deployTask {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	return tr.tasks[id]
}

func (tr *taskRegistry) list() []*deployTask {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	out := make([]*deployTask, 0, len(tr.tasks))
	for _, t := range tr.tasks {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.After(out[j].Created) })
	return out
}

// TaskSummaryData is one entry of the task_status listing (no task_id
// given). Step is the last published step for running tasks.
type TaskSummaryData struct {
	TaskID    string `json:"task_id"`
	Tool      string `json:"tool"`
	Step      string `json:"step"`
	Done      bool   `json:"done"`
	Error     string `json:"error,omitempty"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// TaskStatusData is the full task_status reply for one task.
type TaskStatusData struct {
	TaskID    string       `json:"task_id"`
	Tool      string       `json:"tool"`
	Done      bool         `json:"done"`
	Running   bool         `json:"running"`
	CreatedAt string       `json:"created_at"`
	UpdatedAt string       `json:"updated_at"`
	Deploy    *DeployData  `json:"deploy,omitempty"`
	Restart   *RestartData `json:"restart,omitempty"`
	Error     string       `json:"error,omitempty"`
}

// taskStatus implements the "task_status" action: with task_id, the full
// snapshot of that task; without, a listing of every tracked task so the
// caller can discover an id after the original call's transport died.
func (r *Registry) taskStatus(ctx context.Context, params json.RawMessage) (json.RawMessage, error) {
	p, err := decodeParams(params)
	if err != nil {
		return nil, err
	}
	id, _ := p["task_id"].(string)
	if id == "" {
		summaries := []TaskSummaryData{}
		for _, t := range r.tasks.list() {
			data, updated, done := t.snapshot()
			summaries = append(summaries, TaskSummaryData{
				TaskID:    t.ID,
				Tool:      t.Tool,
				Step:      data.Step,
				Done:      done,
				Error:     data.Error,
				CreatedAt: t.Created.UTC().Format(time.RFC3339),
				UpdatedAt: updated.UTC().Format(time.RFC3339),
			})
		}
		return mustMarshal(map[string]any{"tasks": summaries}), nil
	}
	t := r.tasks.get(id)
	if t == nil {
		return nil, fmt.Errorf("task_status: unknown task_id %q (finished tasks are pruned after %d entries)", id, maxTrackedTasks)
	}
	data, updated, done := t.snapshot()
	out := TaskStatusData{
		TaskID:    t.ID,
		Tool:      t.Tool,
		Done:      done,
		Running:   !done,
		CreatedAt: t.Created.UTC().Format(time.RFC3339),
		UpdatedAt: updated.UTC().Format(time.RFC3339),
		Deploy:    &data,
	}
	if t.Tool == "restart" {
		out.Deploy = nil
		out.Restart = data.RestartResult
		out.Error = data.Error
	}
	return mustMarshal(out), nil
}

// deployCoreFunc is the synchronous body of deploy, parameterised so the
// task wrapper can feed it an already-registered DeployData (with the
// task id stamped in) and a notify callback fired after every step.
type deployCoreFunc func(ctx context.Context, data *DeployData, notify func())

// runDeployTask launches core as a tracked background task and returns
// immediately. The worker runs on a detached context bounded by a
// generous total budget: a synchronous deploy whose transport timed out
// must still run to completion in the background — that is what
// previously made deploys feel non-deterministic (2026-09-25 report).
func (r *Registry) runDeployTask(ctx context.Context, core deployCoreFunc) *deployTask {
	t := r.tasks.newTask("deploy")
	data := &DeployData{Step: "begin", TaskID: t.ID}
	t.publish(*data)
	go func() {
		wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Minute)
		defer cancel()
		core(wctx, data, func() { t.publish(*data) })
		t.finish(*data)
	}()
	return t
}
