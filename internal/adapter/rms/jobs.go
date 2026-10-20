package rms

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/BudEcosystem/dc-twin/internal/adapter/rms/rmspb"
	"github.com/BudEcosystem/dc-twin/internal/engine"
	"github.com/BudEcosystem/dc-twin/internal/model"
)

// Jobs are RMS's asynchronous operations. A batch RPC issues a parent job, returned as the
// batch's job_id, and a child per node. A child is backed by what the twin does for it: firmware
// and switch images by the engine's update tasks on the device (engine.UpdateFirmware, whose
// duration scales with spec.timeScale), the others by a behaviour duration on the engine clock,
// at whose end their effect is applied (a rotated password, a configured fabric, a cleared
// primary). A parent fails once a child has failed, completes once every child has, and an empty
// batch completes on its first poll. Jobs are kept in memory for the life of the twin; RMS keeps
// them for 24 hours.

type jobState int

const (
	running jobState = iota
	completed
	failed
)

func (s jobState) wire() string { return [...]string{"running", "completed", "failed"}[s] }

func (s jobState) execution() rmspb.JobExecutionState {
	return [...]rmspb.JobExecutionState{rmspb.JobExecutionState_JOB_EXECUTION_STATE_RUNNING,
		rmspb.JobExecutionState_JOB_EXECUTION_STATE_COMPLETED, rmspb.JobExecutionState_JOB_EXECUTION_STATE_FAILED}[s]
}

// taskRef is an engine task on a device.
type taskRef struct {
	dev *model.Device
	id  string
}

type job struct {
	id, parent     string
	children       []*job
	kind           string
	rackID, nodeID string // the caller's, echoed
	dev            *model.Device
	created        time.Time
	updated        time.Time
	ends           time.Time
	tasks          []taskRef
	fail           string
	code           rmspb.JobError
	done           func(*job) error // applied once, when the job's time is up
	step           func(*job, time.Time) (jobState, string)
	desc           string // what the job is doing while it runs
	result         string // result_json once completed
	state          jobState
	settled        bool
}

type tracker struct {
	mu   sync.Mutex
	emu  sync.Mutex // one evaluation at a time, so a completion applies once
	e    *engine.Engine
	run  int64
	n    int
	jobs map[string]*job
}

func newTracker(e *engine.Engine) *tracker {
	return &tracker{e: e, run: time.Now().Unix(), jobs: map[string]*job{}}
}

func (t *tracker) nextID() string {
	t.n++
	return fmt.Sprintf("dctwin-rms-%d-%d", t.run, t.n)
}

// add registers a job (and its children) and returns it.
func (t *tracker) add(j *job) *job {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.e.Now()
	var reg func(j *job, parent string)
	reg = func(j *job, parent string) {
		j.id, j.parent, j.created, j.updated = t.nextID(), parent, now, now
		t.jobs[j.id] = j
		for _, c := range j.children {
			reg(c, j.id)
		}
	}
	reg(j, "")
	return j
}

func (t *tracker) get(id string) *job {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.jobs[id]
}

// settle evaluates every job that is not yet terminal, so effects apply when their time is up
// even if nobody polls.
func (t *tracker) settle() {
	t.mu.Lock()
	var open []*job
	for _, j := range t.jobs {
		if !j.settled {
			open = append(open, j)
		}
	}
	t.mu.Unlock()
	sort.Slice(open, func(i, k int) bool { return open[i].id < open[k].id })
	t.emu.Lock()
	defer t.emu.Unlock()
	for _, j := range open {
		t.eval(j)
	}
}

// evaluate is eval for callers outside the tracker.
func (t *tracker) evaluate(j *job) jobState {
	t.emu.Lock()
	defer t.emu.Unlock()
	return t.eval(j)
}

// eval brings a job's state up to date and returns it; callers hold emu.
func (t *tracker) eval(j *job) jobState {
	t.mu.Lock()
	if j.settled {
		s := j.state
		t.mu.Unlock()
		return s
	}
	t.mu.Unlock()
	now := t.e.Now()
	var st jobState
	var msg string
	switch {
	case len(j.children) > 0:
		var bad []string
		all := true
		for _, c := range j.children {
			switch t.eval(c) {
			case failed:
				bad = append(bad, fmt.Sprintf("%s: %s", c.nodeID, c.fail))
			case running:
				all = false
			}
		}
		switch {
		case len(bad) > 0:
			st, msg = failed, fmt.Sprintf("%d of %d nodes failed: %s", len(bad), len(j.children), strings.Join(bad, "; "))
		case all:
			st = completed
		}
	case j.fail != "":
		st, msg = failed, j.fail
	case j.step != nil:
		st, msg = j.step(j, now)
	case len(j.tasks) > 0:
		st = completed
		t.e.Read(func(*model.Model) {
			for _, r := range j.tasks {
				found := false
				for _, task := range r.dev.State.Tasks {
					if task.ID != r.id {
						continue
					}
					found = true
					switch task.State {
					case "Completed":
					case "Exception", "Killed", "Cancelled":
						st, msg = failed, fmt.Sprintf("%s: %s", r.dev.Path, task.Message)
					default:
						if st != failed {
							st = running
						}
					}
				}
				if !found && st != failed {
					st = running
				}
			}
		})
	default:
		if now.Before(j.ends) {
			st = running
		} else {
			st = completed
		}
	}
	if st == completed && j.done != nil {
		if err := j.done(j); err != nil {
			st, msg = failed, err.Error()
		}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if st != running || j.state != running {
		j.updated = now
	}
	j.state = st
	if st == failed {
		if j.fail == "" {
			j.fail = msg
		}
		if j.code == rmspb.JobError_JOB_ERROR_UNSPECIFIED {
			j.code = rmspb.JobError_JOB_ERROR_OTHER
		}
	}
	if st == running && msg != "" {
		j.desc = msg
	}
	j.settled = st != running
	return st
}

// status is a job as GetJobStatus reports it.
func (t *tracker) status(j *job) *rmspb.JobStatus {
	st := t.evaluate(j)
	t.mu.Lock()
	defer t.mu.Unlock()
	js := &rmspb.JobStatus{JobId: j.id, ExecutionState: st.execution(), ErrorCode: rmspb.JobError_JOB_ERROR_UNSPECIFIED,
		StateDescription: j.describe(st), CreatedAt: timestamppb.New(j.created), UpdatedAt: timestamppb.New(j.updated)}
	if j.parent != "" {
		p := j.parent
		js.ParentJobId = &p
	}
	for _, c := range j.children {
		js.ChildJobIds = append(js.ChildJobIds, c.id)
	}
	if st == failed {
		js.ErrorMessage, js.ErrorCode = j.fail, j.code
	}
	if st != running {
		js.ResultJson = j.result
	}
	if j.rackID != "" {
		r := j.rackID
		js.RackId = &r
	}
	if j.nodeID != "" && len(j.children) == 0 {
		n := j.nodeID
		js.NodeId = &n
	}
	return js
}

func (j *job) describe(st jobState) string {
	switch st {
	case completed:
		return j.kind + " completed"
	case failed:
		return j.kind + " failed"
	}
	if j.desc != "" {
		return j.desc
	}
	return j.kind + " running"
}
