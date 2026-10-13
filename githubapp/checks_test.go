package githubapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const sha = "0123456789abcdef0123456789abcdef01234567"

func checkRequest() CheckRunRequest {
	return CheckRunRequest{
		Name: "lux-gha", HeadSHA: sha, ExternalID: "run_1", Status: "completed", Conclusion: "failure",
		CompletedAt: time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC),
		Output:      CheckRunOutput{Title: `Lux stopped job "build": disk limit exceeded`, Summary: "route default"},
	}
}

func TestCreateCheckRunSendsCompletedCheck(t *testing.T) {
	c, _, _ := newTestClient(t, pkcs1PEM(testKey), func(w http.ResponseWriter, r *http.Request) {
		if !requireInstallationToken(w, r, "ghs_token1") {
			return
		}
		if r.Method != "POST" || r.URL.EscapedPath() != "/repos/octo-org/app.repo/check-runs" || r.Header.Get("Content-Type") != "application/json" {
			http.Error(w, "route", 404)
			return
		}
		var body map[string]any
		raw, _ := io.ReadAll(r.Body)
		json.Unmarshal(raw, &body)
		want := map[string]any{
			"name": "lux-gha", "head_sha": sha, "external_id": "run_1", "status": "completed", "conclusion": "failure",
			"completed_at": "2026-10-09T12:00:00Z",
			"output":       map[string]any{"title": `Lux stopped job "build": disk limit exceeded`, "summary": "route default"},
		}
		if !reflect.DeepEqual(body, want) {
			t.Errorf("body %s", raw)
			http.Error(w, "body", 422)
			return
		}
		w.WriteHeader(201)
		w.Write([]byte(`{"id":77,"name":"lux-gha","head_sha":"` + sha + `","external_id":"run_1","status":"completed","conclusion":"failure"}`))
	})
	got, err := c.CreateCheckRun(context.Background(), repo, checkRequest())
	if err != nil || got.ID != 77 || got.ExternalID != "run_1" || *got.Conclusion != "failure" {
		t.Fatalf("got=%+v err=%v", got, err)
	}
}

func TestCreateCheckRunValidatesBeforeSending(t *testing.T) {
	var calls atomic.Int32
	c, _, _ := newTestClient(t, pkcs1PEM(testKey), func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(201)
		w.Write([]byte(`{}`))
	})
	edit := func(f func(*CheckRunRequest)) CheckRunRequest { r := checkRequest(); f(&r); return r }
	for name, req := range map[string]CheckRunRequest{
		"no name":         edit(func(r *CheckRunRequest) { r.Name = "" }),
		"short sha":       edit(func(r *CheckRunRequest) { r.HeadSHA = "abc" }),
		"branch ref":      edit(func(r *CheckRunRequest) { r.HeadSHA = "main" }),
		"in progress":     edit(func(r *CheckRunRequest) { r.Status = "in_progress" }),
		"no conclusion":   edit(func(r *CheckRunRequest) { r.Conclusion = "" }),
		"no completed_at": edit(func(r *CheckRunRequest) { r.CompletedAt = time.Time{} }),
		"no title":        edit(func(r *CheckRunRequest) { r.Output.Title = "" }),
		"long title":      edit(func(r *CheckRunRequest) { r.Output.Title = strings.Repeat("x", 256) }),
		"multibyte title": edit(func(r *CheckRunRequest) { r.Output.Title = strings.Repeat("界", 85) + "x" }),
		"long summary":    edit(func(r *CheckRunRequest) { r.Output.Summary = strings.Repeat("x", 65536) }),
	} {
		if _, err := c.CreateCheckRun(context.Background(), repo, req); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("%d requests sent for invalid input", calls.Load())
	}
	// A 201 without an id is not a created check run.
	if _, err := c.CreateCheckRun(context.Background(), repo, checkRequest()); err == nil {
		t.Fatal("accepted a response without id")
	}
}

func TestCreateCheckRunAccepts255ByteTitles(t *testing.T) {
	for _, title := range []string{strings.Repeat("x", 255), strings.Repeat("界", 85)} {
		t.Run(title[:3], func(t *testing.T) {
			var calls atomic.Int32
			c, _, _ := newTestClient(t, pkcs1PEM(testKey), func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if !requireInstallationToken(w, r, "ghs_token1") {
					return
				}
				if r.Method != "POST" || r.URL.EscapedPath() != "/repos/octo-org/app.repo/check-runs" || r.URL.RawQuery != "" || r.Header.Get("Content-Type") != "application/json" {
					t.Error("unexpected request route")
					http.Error(w, "route", 404)
					return
				}
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				want := map[string]any{
					"name": "lux-gha", "head_sha": sha, "external_id": "run_1", "status": "completed", "conclusion": "failure",
					"completed_at": "2026-10-09T12:00:00Z",
					"output":       map[string]any{"title": title, "summary": "route default"},
				}
				if !reflect.DeepEqual(body, want) {
					t.Errorf("body %+v, want %+v", body, want)
				}
				w.WriteHeader(201)
				w.Write([]byte(`{"id":77}`))
			})
			req := checkRequest()
			req.Output.Title = title
			got, err := c.CreateCheckRun(context.Background(), repo, req)
			if err != nil || got.ID != 77 || calls.Load() != 1 {
				t.Fatalf("got %+v err %v calls %d", got, err, calls.Load())
			}
		})
	}
}

func TestListCheckRunsForRefFiltersByName(t *testing.T) {
	c, _, _ := newTestClient(t, pkcs1PEM(testKey), func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if r.Method != "GET" || r.URL.Path != "/repos/octo-org/app.repo/commits/"+sha+"/check-runs" ||
			q.Get("check_name") != "lux-gha" || q.Get("filter") != "all" || q.Get("page") != "2" || q.Get("per_page") != "100" {
			http.Error(w, "query", 400)
			return
		}
		w.Header().Set("Link", `<https://api.github.com/x?page=3>; rel="next"`)
		w.Write([]byte(`{"total_count":2,"check_runs":[{"id":1,"name":"lux-gha","external_id":"run_1","status":"completed","conclusion":"failure"},{"id":2,"name":"lux-gha","external_id":""}]}`))
	})
	page, err := c.ListCheckRunsForRef(context.Background(), repo, sha, CheckRunListOptions{PageOptions{Page: 2, PerPage: 100}, "lux-gha"})
	if err != nil || !page.HasNext || len(page.CheckRuns) != 2 || page.CheckRuns[0].ExternalID != "run_1" || page.CheckRuns[1].Conclusion != nil {
		t.Fatalf("page=%+v err=%v", page, err)
	}
	if _, err := c.ListCheckRunsForRef(context.Background(), repo, "main", CheckRunListOptions{}); err == nil {
		t.Fatal("accepted a branch name")
	}
}

func TestForceCancelWorkflowRun(t *testing.T) {
	c, _, _ := newTestClient(t, pkcs1PEM(testKey), func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST" && r.URL.Path == "/repos/octo-org/app.repo/actions/runs/5/force-cancel":
			w.WriteHeader(202)
			w.Write([]byte(`{}`))
		case r.Method == "POST" && r.URL.Path == "/repos/octo-org/app.repo/actions/runs/6/force-cancel":
			http.Error(w, `{"message":"Cannot cancel a workflow run that is completed."}`, 409)
		case r.Method == "POST" && r.URL.Path == "/repos/octo-org/app.repo/actions/runs/7/force-cancel":
			w.Header().Set("X-RateLimit-Remaining", "4999")
			http.Error(w, `{"message":"Resource not accessible by integration"}`, 403)
		case r.Method == "POST" && r.URL.Path == "/repos/octo-org/app.repo/actions/runs/8/force-cancel":
			w.Header().Set("X-RateLimit-Remaining", "0")
			http.Error(w, `{"message":"API rate limit exceeded"}`, 403)
		default:
			http.NotFound(w, r)
		}
	})
	if err := c.ForceCancelWorkflowRun(context.Background(), repo, 5); err != nil {
		t.Fatal(err)
	}
	var apiErr *APIError
	if err := c.ForceCancelWorkflowRun(context.Background(), repo, 6); !errors.As(err, &apiErr) || apiErr.StatusCode != 409 {
		t.Fatalf("completed run: err=%v", err)
	}
	if err := c.ForceCancelWorkflowRun(context.Background(), repo, 7); !errors.As(err, &apiErr) || apiErr.StatusCode != 403 || apiErr.RateLimited {
		t.Fatalf("missing permission: err=%v", err)
	}
	if err := c.ForceCancelWorkflowRun(context.Background(), repo, 8); !errors.As(err, &apiErr) || apiErr.StatusCode != 403 || !apiErr.RateLimited {
		t.Fatalf("rate limit: err=%v", err)
	}
	if err := c.ForceCancelWorkflowRun(context.Background(), repo, 0); err == nil {
		t.Fatal("accepted run 0")
	}
}

func TestCheckRunEndpointsReturnAPIErrors(t *testing.T) {
	for _, status := range []int{403, 422} {
		for _, method := range []string{"GET", "POST"} {
			t.Run(fmt.Sprintf("%s/%d", method, status), func(t *testing.T) {
				c, _, _ := newTestClient(t, pkcs1PEM(testKey), func(w http.ResponseWriter, r *http.Request) {
					if r.Method != method {
						t.Errorf("method %s", r.Method)
					}
					w.Header().Set("X-RateLimit-Remaining", "4999")
					http.Error(w, `{"message":"refused"}`, status)
				})
				var err error
				if method == "POST" {
					_, err = c.CreateCheckRun(context.Background(), repo, checkRequest())
				} else {
					_, err = c.ListCheckRunsForRef(context.Background(), repo, sha, CheckRunListOptions{})
				}
				var apiErr *APIError
				if !errors.As(err, &apiErr) || apiErr.StatusCode != status || apiErr.RateLimited {
					t.Fatalf("err=%v", err)
				}
			})
		}
	}
}
