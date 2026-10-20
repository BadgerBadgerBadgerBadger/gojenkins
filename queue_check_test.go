package gojenkins

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	fakeAPISuffix       = "/api/json"
	fakeQueuePath       = "/queue"
	fakeNewQueueID      = int64(99)
	fakeLocationHdr     = "Location"
	fakeBuildWithParams = "/buildWithParameters"
	// Invoke's IsRunning reads the job's last build, at /job/<name>/<number>.
	fakeLastBuildNumber = 1
	fakeLastBuildPath   = "/1"
	deployJobBase       = "/job/deploy"
	folderJobBase       = "/job/team/job/deploy"
	plainJobBase        = "/job/plain"
	otherJenkinsRoot    = "https://jenkins.example.com/ci"
)

type fakeParam struct {
	name  string
	value interface{}
}

type fakeQueueItem struct {
	id      int64
	taskURL string
	params  []fakeParam
}

type fakeJob struct {
	defaults []fakeParam // parameter definitions with their default values
}

// fakeJenkins serves just enough of the Jenkins API for InvokeSimple/Invoke.
type fakeJenkins struct {
	jobs  map[string]fakeJob // keyed by job Base, e.g. /job/team/job/deploy
	queue []fakeQueueItem

	mu    sync.Mutex
	posts []string
}

// inQueue is deliberately loose (a leaf-name suffix match), like Jenkins'
// flag is from the client's point of view: it only gates the queue fetch, and
// the URL matching under test must then reject the wrong folder.
func (f *fakeJenkins) inQueue(base string) bool {
	for _, item := range f.queue {
		if strings.HasSuffix(strings.TrimSuffix(item.taskURL, "/"), base) {
			return true
		}
	}
	return false
}

func (f *fakeJenkins) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	if r.Method == http.MethodPost {
		f.mu.Lock()
		f.posts = append(f.posts, path)
		f.mu.Unlock()
		w.Header().Set(fakeLocationHdr, fmt.Sprintf("http://%s/queue/item/%d/", r.Host, fakeNewQueueID))
		w.WriteHeader(http.StatusCreated)
		return
	}

	if path == fakeQueuePath+fakeAPISuffix {
		items := []interface{}{}
		for _, item := range f.queue {
			params := []interface{}{}
			for _, p := range item.params {
				params = append(params, map[string]interface{}{"name": p.name, "value": p.value})
			}
			items = append(items, map[string]interface{}{
				"id":   item.id,
				"task": map[string]interface{}{"name": item.taskURL, "url": item.taskURL},
				"actions": []interface{}{
					map[string]interface{}{},
					map[string]interface{}{"_class": "hudson.model.ParametersAction", "parameters": params},
				},
			})
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"items": items})
		return
	}

	base := strings.TrimSuffix(path, fakeAPISuffix)
	if _, ok := f.jobs[strings.TrimSuffix(base, fakeLastBuildPath)]; ok && strings.HasSuffix(base, fakeLastBuildPath) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"number": fakeLastBuildNumber, "building": false})
		return
	}
	job, ok := f.jobs[base]
	if !ok {
		http.NotFound(w, r)
		return
	}
	definitions := []interface{}{}
	for _, d := range job.defaults {
		definitions = append(definitions, map[string]interface{}{
			"name":                  d.name,
			"defaultParameterValue": map[string]interface{}{"name": d.name, "value": d.value},
		})
	}
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"name":      base[strings.LastIndex(base, "/")+1:],
		"inQueue":   f.inQueue(base),
		"lastBuild": map[string]interface{}{"number": fakeLastBuildNumber},
		"property":  []interface{}{map[string]interface{}{"parameterDefinitions": definitions}},
	})
}

var deployDefaults = []fakeParam{
	{"ENVIRONMENT", "GALACTICA"},
	{"JOB_NAME", ""},
	{"DRY_RUN", false},
}

func deployJobs() map[string]fakeJob {
	return map[string]fakeJob{
		deployJobBase: {defaults: deployDefaults},
		folderJobBase: {defaults: deployDefaults},
		plainJobBase:  {},
	}
}

func queued(id int64, base string, params ...fakeParam) fakeQueueItem {
	return fakeQueueItem{id: id, taskURL: otherJenkinsRoot + base + "/", params: params}
}

func TestInvokeSimpleQueueCheck(t *testing.T) {
	v18 := map[string]string{"ENVIRONMENT": "VALKYRIE", "JOB_NAME": "shiva", "DRY_RUN": "false"}
	v18Queued := []fakeParam{{"ENVIRONMENT", "VALKYRIE"}, {"JOB_NAME", "shiva"}, {"DRY_RUN", false}}

	tests := []struct {
		name      string
		job       string // name as passed to BuildJob
		params    map[string]string
		queue     []fakeQueueItem
		wantDupID int64 // 0 means the build must launch
		// wantEndpoint is the launch endpoint when the build launches.
		wantEndpoint string
	}{
		{
			name:         "nothing queued launches",
			wantEndpoint: fakeBuildWithParams,
			job:          "deploy",
			params:       v18,
		},
		{
			name:         "different params queued launches",
			wantEndpoint: fakeBuildWithParams,
			job:          "deploy",
			params:       v18,
			queue: []fakeQueueItem{
				queued(7, deployJobBase, fakeParam{"ENVIRONMENT", "USSENTERPRISE"}, fakeParam{"JOB_NAME", "shiva"}, fakeParam{"DRY_RUN", false}),
				queued(8, deployJobBase, fakeParam{"ENVIRONMENT", "VALKYRIE"}, fakeParam{"JOB_NAME", "users"}, fakeParam{"DRY_RUN", false}),
				queued(9, deployJobBase, fakeParam{"ENVIRONMENT", "VALKYRIE"}, fakeParam{"JOB_NAME", "shiva"}, fakeParam{"DRY_RUN", true}),
			},
		},
		{
			name:      "identical params queued is refused",
			job:       "deploy",
			params:    v18,
			queue:     []fakeQueueItem{queued(7, deployJobBase, fakeParam{"ENVIRONMENT", "PEGASUS"}), queued(8, deployJobBase, v18Queued...)},
			wantDupID: 8,
		},
		{
			name:      "identical except an omitted default is refused",
			job:       "deploy",
			params:    map[string]string{"ENVIRONMENT": "VALKYRIE", "JOB_NAME": "shiva"},
			queue:     []fakeQueueItem{queued(8, deployJobBase, v18Queued...)},
			wantDupID: 8,
		},
		{
			name:      "undefined requested params are ignored",
			job:       "deploy",
			params:    map[string]string{"ENVIRONMENT": "VALKYRIE", "JOB_NAME": "shiva", "NOT_A_PARAM": "x"},
			queue:     []fakeQueueItem{queued(8, deployJobBase, v18Queued...)},
			wantDupID: 8,
		},
		{
			name:      "folder job identical params queued is refused",
			job:       "team/job/deploy",
			params:    v18,
			queue:     []fakeQueueItem{queued(8, folderJobBase, v18Queued...)},
			wantDupID: 8,
		},
		{
			name:         "same leaf name in another folder launches",
			wantEndpoint: fakeBuildWithParams,
			job:          "deploy",
			params:       v18,
			queue:        []fakeQueueItem{queued(8, folderJobBase, v18Queued...)},
		},
		{
			name:      "parameterless job with an item queued is refused",
			job:       "plain",
			queue:     []fakeQueueItem{queued(8, plainJobBase)},
			wantDupID: 8,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := &fakeJenkins{jobs: deployJobs(), queue: tt.queue}
			server := httptest.NewServer(fake)
			defer server.Close()

			jenkins := CreateJenkins(nil, server.URL)
			id, err := jenkins.BuildJob(context.Background(), tt.job, tt.params)

			if tt.wantDupID == 0 {
				require.NoError(t, err)
				assert.Equal(t, fakeNewQueueID, id)
				require.Len(t, fake.posts, 1)
				assert.True(t, strings.HasSuffix(fake.posts[0], tt.wantEndpoint), "posted to %s", fake.posts[0])
				return
			}

			assert.True(t, errors.Is(err, ErrIdenticalBuildQueued), "got %v", err)
			var dup *IdenticalBuildQueuedError
			require.True(t, errors.As(err, &dup))
			assert.Equal(t, tt.wantDupID, dup.QueueID)
			assert.Zero(t, id)
			assert.Empty(t, fake.posts)
		})
	}
}

func TestInvokeQueueCheck(t *testing.T) {
	v18Queued := []fakeParam{{"ENVIRONMENT", "VALKYRIE"}, {"JOB_NAME", "shiva"}, {"DRY_RUN", false}}
	fake := &fakeJenkins{jobs: deployJobs(), queue: []fakeQueueItem{queued(8, deployJobBase, v18Queued...)}}
	server := httptest.NewServer(fake)
	defer server.Close()

	ctx := context.Background()
	jenkins := CreateJenkins(nil, server.URL)
	job := Job{Jenkins: jenkins, Raw: new(JobResponse), Base: deployJobBase}

	ok, err := job.Invoke(ctx, nil, false, map[string]string{"ENVIRONMENT": "VALKYRIE", "JOB_NAME": "shiva"}, "", "")
	assert.False(t, ok)
	assert.True(t, errors.Is(err, ErrIdenticalBuildQueued), "got %v", err)
	assert.Empty(t, fake.posts)

	ok, err = job.Invoke(ctx, nil, false, map[string]string{"ENVIRONMENT": "PEGASUS", "JOB_NAME": "shiva"}, "", "")
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Len(t, fake.posts, 1)
}
