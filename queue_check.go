// Copyright 2015 Vadim Kravcenko
// Modified 2026 by BadgerBadgerBadgerBadger: parameter-aware queue check.
//
// Licensed under the Apache License, Version 2.0 (the "License"): you may
// not use this file except in compliance with the License. You may obtain
// a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS, WITHOUT
// WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied. See the
// License for the specific language governing permissions and limitations
// under the License.

package gojenkins

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

const (
	// queueTreeQuery limits /queue/api/json to the fields the check reads.
	queueTreeQuery = "items[id,task[name,url],actions[parameters[name,value]]]"
	treeQueryKey   = "tree"
	jobPathSegment = "job"
)

// ErrIdenticalBuildQueued is matched (errors.Is) by an *IdenticalBuildQueuedError.
var ErrIdenticalBuildQueued = errors.New("a build with identical parameters is already queued")

// IdenticalBuildQueuedError reports that a launch was refused because the job
// already has a queued item with exactly the requested parameters.
type IdenticalBuildQueuedError struct {
	Job     string
	QueueID int64
}

func (e *IdenticalBuildQueuedError) Error() string {
	return fmt.Sprintf("job %q: %s (queue item %d)", e.Job, ErrIdenticalBuildQueued.Error(), e.QueueID)
}

func (e *IdenticalBuildQueuedError) Is(target error) bool {
	return target == ErrIdenticalBuildQueued
}

type queueParamsResponse struct {
	Items []struct {
		ID   int64 `json:"id"`
		Task struct {
			Name string `json:"name"`
			URL  string `json:"url"`
		} `json:"task"`
		Actions []struct {
			Parameters []struct {
				Name  string      `json:"name"`
				Value interface{} `json:"value"`
			} `json:"parameters"`
		} `json:"actions"`
	} `json:"items"`
}

// checkQueuedDuplicate returns an *IdenticalBuildQueuedError when the job has
// a queued item whose parameters equal params once the job's defaults are
// filled in. Items with other parameters, and running builds, do not block.
//
// j.Raw must be freshly polled (GetParameters does that) and definitions are
// the job's parameter definitions. Only defined parameters are compared,
// because Jenkins drops undefined ones before queueing. Password values are
// masked in the queue, so they never match and the launch goes ahead.
//
// The check and the following POST are not atomic. That is harmless: Jenkins
// itself folds an identical request into the existing queue item.
func (j *Job) checkQueuedDuplicate(ctx context.Context, params map[string]string, definitions []ParameterDefinition) error {
	if !j.Raw.InQueue {
		return nil
	}

	queue := new(queueParamsResponse)
	resp, err := j.Jenkins.Requester.GetJSON(ctx, j.Jenkins.GetQueueUrl(), queue, map[string]string{treeQueryKey: queueTreeQuery})
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("could not read the Jenkins queue: %s", resp.Status)
	}

	want := requestedParameters(params, definitions)
	for _, item := range queue.Items {
		if !j.isTaskURL(item.Task.URL) {
			continue
		}

		got := map[string]string{}
		for _, action := range item.Actions {
			for _, p := range action.Parameters {
				if p.Value == nil || !isDefined(p.Name, definitions) {
					continue
				}
				got[p.Name] = parameterString(p.Value)
			}
		}

		if equalParameters(want, got) {
			name := j.Raw.FullName
			if name == "" {
				name = j.GetName()
			}
			return &IdenticalBuildQueuedError{Job: name, QueueID: item.ID}
		}
	}

	return nil
}

// requestedParameters is what Jenkins would queue for params: every defined
// parameter, taking the job's default where params leaves it out.
func requestedParameters(params map[string]string, definitions []ParameterDefinition) map[string]string {
	effective := map[string]string{}
	for _, d := range definitions {
		if v, ok := params[d.Name]; ok {
			effective[d.Name] = v
			continue
		}
		if d.DefaultParameterValue.Value != nil {
			effective[d.Name] = parameterString(d.DefaultParameterValue.Value)
		}
	}
	return effective
}

func isDefined(name string, definitions []ParameterDefinition) bool {
	for _, d := range definitions {
		if d.Name == name {
			return true
		}
	}
	return false
}

// parameterString renders a JSON parameter value the way it is posted as a
// form value, so a boolean true compares equal to "true".
func parameterString(v interface{}) string {
	switch v := v.(type) {
	case string:
		return v
	case bool:
		return strconv.FormatBool(v)
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	default:
		return fmt.Sprint(v)
	}
}

func equalParameters(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}

// isTaskURL reports whether a queue item's task URL is this job. It matches
// the trailing /job/... segments against j.Base, so a Jenkins root URL or
// context path that differs from the client's base still matches, while a
// same-named job in another folder does not.
func (j *Job) isTaskURL(taskURL string) bool {
	u, err := url.Parse(taskURL)
	if err != nil {
		return false
	}

	task := pathSegments(u.Path)
	base := pathSegments(j.Base)
	if len(base) == 0 || len(task) < len(base) {
		return false
	}

	prefix := task[:len(task)-len(base)]
	for i, s := range base {
		if task[len(prefix)+i] != s {
			return false
		}
	}

	// A prefix ending in /job/<folder> means the task is nested deeper.
	return len(prefix) < 2 || prefix[len(prefix)-2] != jobPathSegment
}

func pathSegments(p string) []string {
	p = strings.Trim(p, "/")
	if p == "" {
		return nil
	}
	return strings.Split(p, "/")
}
