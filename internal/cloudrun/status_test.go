package cloudrun

import (
	"context"
	"net/http"
	"strings"
	"testing"

	run "google.golang.org/api/run/v1"
)

// readyService は Ready なサービスの API レスポンス相当。
func readyService() *run.Service {
	return &run.Service{
		ApiVersion: manifestAPIVersion,
		Kind:       manifestKind,
		Metadata:   &run.ObjectMeta{Name: "my-svc", Namespace: testProject, Generation: 7},
		Spec: &run.ServiceSpec{Template: &run.RevisionTemplate{
			Spec: &run.RevisionSpec{Containers: []*run.Container{{Image: "gcr.io/project/image:tag"}}},
		}},
		Status: &run.ServiceStatus{
			Url:                       "https://my-svc.a.run.app",
			LatestReadyRevisionName:   "my-svc-00007-abc",
			LatestCreatedRevisionName: "my-svc-00007-abc",
			ObservedGeneration:        7,
			Conditions: []*run.GoogleCloudRunV1Condition{
				{Type: "Ready", Status: "True", LastTransitionTime: "2026-08-22T00:00:00Z"},
				{Type: "ConfigurationsReady", Status: "True"},
				{Type: "RoutesReady", Status: "True"},
			},
			Traffic: []*run.TrafficTarget{
				{RevisionName: "my-svc-00007-abc", Percent: 100, LatestRevision: true},
			},
		},
	}
}

func TestNewStatus(t *testing.T) {
	got := newStatus(readyService())

	if got.Service != "my-svc" {
		t.Errorf("Service = %q, want %q", got.Service, "my-svc")
	}
	if got.URL != "https://my-svc.a.run.app" {
		t.Errorf("URL = %q", got.URL)
	}
	if got.LatestReadyRevision != "my-svc-00007-abc" || got.LatestCreatedRevision != "my-svc-00007-abc" {
		t.Errorf("revisions = %q / %q", got.LatestReadyRevision, got.LatestCreatedRevision)
	}
	if got.Generation != 7 || got.ObservedGeneration != 7 {
		t.Errorf("generation = %d (observed %d), want 7 / 7", got.Generation, got.ObservedGeneration)
	}
	if len(got.Conditions) != 3 {
		t.Fatalf("Conditions = %+v, want 3 entries", got.Conditions)
	}
	if got.Conditions[0].LastTransitionTime != "2026-08-22T00:00:00Z" {
		t.Errorf("LastTransitionTime = %q", got.Conditions[0].LastTransitionTime)
	}
	if len(got.Traffic) != 1 || got.Traffic[0].Percent != 100 || !got.Traffic[0].Latest {
		t.Errorf("Traffic = %+v", got.Traffic)
	}
}

func TestNewStatusIsNilSafe(t *testing.T) {
	// nil や status 未設定 (作成直後など) でも panic しない。
	for _, tt := range []struct {
		name string
		obj  *run.Service
	}{
		{name: "nil service", obj: nil},
		{name: "no status", obj: &run.Service{Metadata: &run.ObjectMeta{Name: "my-svc"}}},
		{name: "no metadata", obj: &run.Service{Status: &run.ServiceStatus{}}},
		{name: "nil entries", obj: &run.Service{Status: &run.ServiceStatus{
			Conditions: []*run.GoogleCloudRunV1Condition{nil},
			Traffic:    []*run.TrafficTarget{nil},
		}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := newStatus(tt.obj)
			if got == nil {
				t.Fatal("newStatus() = nil, want a value")
			}
			if len(got.Conditions) != 0 || len(got.Traffic) != 0 {
				t.Errorf("nil entries should be skipped: %+v", got)
			}
		})
	}
}

func TestStatusReady(t *testing.T) {
	s := newStatus(readyService())
	c := s.Ready()
	if c == nil {
		t.Fatal("Ready() = nil, want the Ready condition")
	}
	if c.Status != "True" {
		t.Errorf("Ready().Status = %q, want True", c.Status)
	}

	// Ready 条件が無ければ nil。
	if (&Status{Conditions: []Condition{{Type: "RoutesReady", Status: "True"}}}).Ready() != nil {
		t.Error("Ready() should be nil when there is no Ready condition")
	}
}

func TestStatusText(t *testing.T) {
	want := `Service:         my-svc
URL:             https://my-svc.a.run.app
Ready:           True
Latest ready:    my-svc-00007-abc
Latest created:  my-svc-00007-abc
Generation:      7 (observed 7)
Traffic:
  100%  my-svc-00007-abc
Conditions:
  Ready                True
  ConfigurationsReady  True
  RoutesReady          True
`
	if got := newStatus(readyService()).Text(); got != want {
		t.Errorf("Text() mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestStatusTextWhenNotReady(t *testing.T) {
	obj := readyService()
	obj.Status.Conditions = []*run.GoogleCloudRunV1Condition{
		{
			Type:    "Ready",
			Status:  "False",
			Reason:  "ConflictingRevisionName",
			Message: "Found a conflicting revision name my-svc-00007-abc.",
		},
	}
	obj.Status.ObservedGeneration = 6

	got := newStatus(obj).Text()
	for _, want := range []string{
		"Ready:           False (ConflictingRevisionName)",
		"Message:         Found a conflicting revision name my-svc-00007-abc.",
		"Generation:      7 (observed 6)",
		"  Ready  False  ConflictingRevisionName",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("Text() should contain %q:\n%s", want, got)
		}
	}
}

func TestStatusTextTrafficDetails(t *testing.T) {
	obj := readyService()
	obj.Status.Traffic = []*run.TrafficTarget{
		{RevisionName: "my-svc-00007-abc", Percent: 90},
		{RevisionName: "my-svc-00008-def", Percent: 10, Tag: "canary"},
		{Percent: 0, LatestRevision: true},
	}

	got := newStatus(obj).Text()
	for _, want := range []string{
		"   90%  my-svc-00007-abc\n",
		"   10%  my-svc-00008-def  (tag: canary)\n",
		"    0%  (latest)\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("Text() should contain %q:\n%s", want, got)
		}
	}
}

func TestStatusTextIsEmptyForAnEmptyStatus(t *testing.T) {
	// 何も無いときに "Generation: 0 (observed 0)" のような無意味な行を出さない。
	if got := (&Status{}).Text(); got != "" {
		t.Errorf("Text() = %q, want empty", got)
	}
}

func TestClientStatus(t *testing.T) {
	c, api := newTestClient(t, func(r *http.Request) (int, interface{}) {
		return http.StatusOK, readyService()
	})

	got, err := c.Status(context.Background(), "my-svc")
	if err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	if got.Service != "my-svc" || got.Ready() == nil || got.Ready().Status != "True" {
		t.Errorf("Status() = %+v", got)
	}

	reqs := api.recorded()
	wantPath := "/apis/serving.knative.dev/v1/namespaces/test-project/services/my-svc"
	if len(reqs) != 1 || reqs[0].Method != http.MethodGet || reqs[0].Path != wantPath {
		t.Errorf("requests = %+v, want a single GET to %q", reqs, wantPath)
	}
}

func TestClientStatusPropagatesErrors(t *testing.T) {
	c, _ := newTestClient(t, nil) // 既定の handler は 404
	if _, err := c.Status(context.Background(), "missing"); err == nil {
		t.Fatal("Status() error = nil, want an error")
	}
}
