//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
	run "google.golang.org/api/run/v1"
)

// The live state is read straight from the Cloud Run v1 API rather than through gcloud, whose
// start-up alone costs about a second and a half per check. The oracle shares only Google's
// generated client with clrnd, never clrnd's own code, so it still checks clrnd from outside.
// It acts as the ADC identity clrnd uses; gcloud is kept for what clrnd must not be the one to
// do (pinning a revision as a third-party writer) and for orphan cleanup.
func newAPI(c settings) (*run.APIService, error) {
	opts := []option.ClientOption{option.WithEndpoint("https://" + c.region + "-run.googleapis.com/")}
	if path := adcPath(c.env); path != "" {
		kind, err := adcType(path)
		if err != nil {
			return nil, err
		}
		opts = append(opts, option.WithAuthCredentialsFile(kind, path))
	}
	// Not the test's context: cleanup still needs the client after an interrupt cancels that.
	return run.NewService(context.Background(), opts...)
}

// adcPath is the ADC file the child processes are given, so the oracle authenticates as they do.
// It is empty when they fall back to gcloud's well-known file, which the default lookup also finds.
func adcPath(env []string) string {
	path := ""
	for _, v := range env {
		if x, ok := strings.CutPrefix(v, "GOOGLE_APPLICATION_CREDENTIALS="); ok {
			path = x
		}
	}
	return path
}

func adcType(path string) (option.CredentialsType, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	var id struct {
		Type string `json:"type"`
	}
	if err = json.Unmarshal(b, &id); err != nil {
		return "", fmt.Errorf("invalid ADC: %w", err)
	}
	switch id.Type {
	case "impersonated_service_account":
		return option.ImpersonatedServiceAccount, nil
	case "service_account":
		return option.ServiceAccount, nil
	case "authorized_user":
		return option.AuthorizedUser, nil
	case "external_account":
		return option.ExternalAccount, nil
	}
	return "", fmt.Errorf("unsupported ADC type %q", id.Type)
}

func isNotFound(err error) bool {
	var gerr *googleapi.Error
	return errors.As(err, &gerr) && gerr.Code == http.StatusNotFound
}

// retryable is what a read may wait out: a rate limit or a server-side failure. gcloud retried
// these on its own, so without this a single 503 would fail a scenario that did nothing wrong.
func retryable(err error) bool {
	var gerr *googleapi.Error
	return errors.As(err, &gerr) && (gerr.Code == http.StatusTooManyRequests || gerr.Code >= 500)
}

// call runs one API request with a bounded context and logs it like a command. Only transient
// errors are retried; a 404 comes back to the caller, which decides whether it means absent.
func call[T any](s *suite, t *testing.T, ctx context.Context, what string, do func(context.Context) (T, error)) (T, error) {
	t.Helper()
	const attempts = 3
	var (
		v   T
		err error
	)
	for i := range attempts {
		start := time.Now()
		rctx, cancel := context.WithTimeout(ctx, time.Minute)
		v, err = do(rctx)
		cancel()
		elapsed := time.Since(start).Round(100 * time.Millisecond)
		status := "ok"
		if err != nil {
			status = err.Error()
		}
		s.record(t, fmt.Sprintf("[%s]\nAPI %s: %s (%s)", t.Name(), what, status, elapsed))
		s.log(t, fmt.Sprintf("API %s: %s (%s)", what, short(status), elapsed))
		if err == nil || !retryable(err) || i == attempts-1 {
			break
		}
		select {
		case <-ctx.Done():
			return v, err
		case <-time.After(2 * time.Second):
		}
	}
	return v, err
}

// short keeps a console line to the status; the transcript holds the full message.
func short(status string) string {
	if len(status) > 120 {
		return status[:120] + "..."
	}
	return status
}

func (s *suite) namespace() string { return "namespaces/" + s.c.project }

func (s *suite) getService(t *testing.T, ctx context.Context) (*run.Service, error) {
	t.Helper()
	return call(s, t, ctx, "get service "+s.service, func(ctx context.Context) (*run.Service, error) {
		return s.api.Namespaces.Services.Get(s.namespace() + "/services/" + s.service).Context(ctx).Do()
	})
}

func (s *suite) getJob(t *testing.T, ctx context.Context) (*run.Job, error) {
	t.Helper()
	return call(s, t, ctx, "get job "+s.job, func(ctx context.Context) (*run.Job, error) {
		return s.api.Namespaces.Jobs.Get(s.namespace() + "/jobs/" + s.job).Context(ctx).Do()
	})
}

func (s *suite) getWorkerPool(t *testing.T, ctx context.Context) (*run.WorkerPool, error) {
	t.Helper()
	return call(s, t, ctx, "get worker pool "+s.pool, func(ctx context.Context) (*run.WorkerPool, error) {
		return s.api.Namespaces.Workerpools.Get(s.namespace() + "/workerpools/" + s.pool).Context(ctx).Do()
	})
}

// scaleWorkerPool sets the instance count the way an external autoscaler does: it rewrites the
// manualInstanceCount annotation on the live worker pool and replaces it, against the version it
// read.
func (s *suite) scaleWorkerPool(t *testing.T, count string) error {
	t.Helper()
	live, err := s.getWorkerPool(t, s.ctx)
	if err != nil {
		return err
	}
	live.Metadata.Annotations["run.googleapis.com/scalingMode"] = "manual"
	live.Metadata.Annotations["run.googleapis.com/manualInstanceCount"] = count
	live.Status = nil
	_, err = call(s, t, s.ctx, "scale worker pool "+s.pool+" to "+count, func(ctx context.Context) (*run.WorkerPool, error) {
		return s.api.Namespaces.Workerpools.ReplaceWorkerPool(s.namespace()+"/workerpools/"+s.pool, live).Context(ctx).Do()
	})
	return err
}

// listWorkerPools lists every worker pool in the project and region, for orphan cleanup: the
// gcloud releases still in use predate its worker-pools commands.
func (s *suite) listWorkerPools(t *testing.T) ([]string, error) {
	t.Helper()
	var names []string
	token := ""
	for {
		page, err := call(s, t, s.ctx, "list worker pools", func(ctx context.Context) (*run.ListWorkerPoolsResponse, error) {
			c := s.api.Namespaces.Workerpools.List(s.namespace()).Context(ctx)
			if token != "" {
				c = c.Continue(token)
			}
			return c.Do()
		})
		if err != nil {
			return nil, err
		}
		for _, item := range page.Items {
			if item != nil && item.Metadata != nil {
				names = append(names, item.Metadata.Name)
			}
		}
		next := ""
		if page.Metadata != nil {
			next = page.Metadata.Continue
		}
		if next == "" {
			return names, nil
		}
		if next == token {
			return nil, errors.New("worker pool list repeated its continue token")
		}
		token = next
	}
}

func (s *suite) getRevision(t *testing.T, name string) (*run.Revision, error) {
	t.Helper()
	return call(s, t, s.ctx, "get revision "+name, func(ctx context.Context) (*run.Revision, error) {
		return s.api.Namespaces.Revisions.Get(s.namespace() + "/revisions/" + name).Context(ctx).Do()
	})
}

func (s *suite) listRevisions(t *testing.T) ([]*run.Revision, error) {
	t.Helper()
	var all []*run.Revision
	token := ""
	for {
		page, err := call(s, t, s.ctx, "list revisions of "+s.service, func(ctx context.Context) (*run.ListRevisionsResponse, error) {
			c := s.api.Namespaces.Revisions.List(s.namespace()).
				LabelSelector("serving.knative.dev/service=" + s.service).Context(ctx)
			if token != "" {
				c = c.Continue(token)
			}
			return c.Do()
		})
		if err != nil {
			return nil, err
		}
		all = append(all, page.Items...)
		next := ""
		if page.Metadata != nil {
			next = page.Metadata.Continue
		}
		if next == "" {
			return all, nil
		}
		if next == token {
			return nil, errors.New("revision list repeated its continue token")
		}
		token = next
	}
}

// deleteResource removes this run's service, job or worker pool. A 404 means it is already gone.
func (s *suite) deleteResource(t *testing.T, ctx context.Context, kind, name string) error {
	t.Helper()
	_, err := call(s, t, ctx, "delete "+kind+" "+name, func(ctx context.Context) (*run.Status, error) {
		switch kind {
		case "job":
			return s.api.Namespaces.Jobs.Delete(s.namespace() + "/jobs/" + name).Context(ctx).Do()
		case "worker pool":
			return s.api.Namespaces.Workerpools.Delete(s.namespace() + "/workerpools/" + name).Context(ctx).Do()
		}
		return s.api.Namespaces.Services.Delete(s.namespace() + "/services/" + name).Context(ctx).Do()
	})
	if isNotFound(err) {
		return nil
	}
	return err
}
