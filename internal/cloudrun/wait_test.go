package cloudrun

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	run "google.golang.org/api/run/v1"
)

// serviceWithReady は Ready 条件と observedGeneration を指定したサービスを組み立てる。
// status が空なら Ready 条件そのものを持たない (作成直後の状態)。
func serviceWithReady(status, reason, message string, observed int64) *run.Service {
	svc := &run.Service{
		ApiVersion: manifestAPIVersion,
		Kind:       manifestKind,
		Metadata:   &run.ObjectMeta{Name: "my-svc", Generation: 3},
		Status:     &run.ServiceStatus{ObservedGeneration: observed},
	}
	if status != "" {
		svc.Status.Conditions = []*run.GoogleCloudRunV1Condition{
			{Type: conditionReady, Status: status, Reason: reason, Message: message},
		}
	}
	return svc
}

// sequenceHandler は呼ばれるたびに次のサービスを返す。最後の要素はそれ以降ずっと返す。
// 呼び出し回数も返すので「何回ポーリングしたか」を検証できる。
func sequenceHandler(objs ...*run.Service) (func(*http.Request) (int, interface{}), func() int) {
	var mu sync.Mutex
	calls := 0
	handler := func(*http.Request) (int, interface{}) {
		mu.Lock()
		defer mu.Unlock()
		obj := objs[min(calls, len(objs)-1)]
		calls++
		return http.StatusOK, obj
	}
	count := func() int {
		mu.Lock()
		defer mu.Unlock()
		return calls
	}
	return handler, count
}

// fastWait はテスト用に待ち時間をほぼ 0 にした WaitOptions を返す。
func fastWait(generation int64) WaitOptions {
	return WaitOptions{Timeout: 5 * time.Second, Interval: time.Millisecond, Generation: generation}
}

func TestWaitDone(t *testing.T) {
	tests := []struct {
		name       string
		status     *Status
		generation int64
		wantDone   bool
		wantErr    string
	}{
		{
			name:     "ready",
			status:   newStatus(serviceWithReady(conditionTrue, "", "", 3)),
			wantDone: true,
		},
		{
			name:     "failed",
			status:   newStatus(serviceWithReady(conditionFalse, "RevisionFailed", "boom", 3)),
			wantDone: true,
			wantErr:  `service "my-svc" failed to become ready: False (RevisionFailed): boom`,
		},
		{
			name:     "still reconciling",
			status:   newStatus(serviceWithReady("Unknown", "Deploying", "", 3)),
			wantDone: false,
		},
		{
			name:     "no Ready condition yet",
			status:   newStatus(serviceWithReady("", "", "", 3)),
			wantDone: false,
		},
		{
			// 前の世代の Ready=True を見て「成功」と誤判定しないこと。
			name:       "generation not observed yet",
			status:     newStatus(serviceWithReady(conditionTrue, "", "", 2)),
			generation: 3,
			wantDone:   false,
		},
		{
			name:       "generation observed",
			status:     newStatus(serviceWithReady(conditionTrue, "", "", 3)),
			generation: 3,
			wantDone:   true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			done, err := waitDone(tt.status, "my-svc", tt.generation)
			if done != tt.wantDone {
				t.Errorf("waitDone() done = %v, want %v", done, tt.wantDone)
			}
			switch {
			case tt.wantErr == "" && err != nil:
				t.Errorf("waitDone() error = %v, want nil", err)
			case tt.wantErr != "" && (err == nil || err.Error() != tt.wantErr):
				t.Errorf("waitDone() error = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestWaitReturnsWhenReady(t *testing.T) {
	handler, calls := sequenceHandler(
		serviceWithReady("", "", "", 0),
		serviceWithReady("Unknown", "Deploying", "", 3),
		serviceWithReady(conditionTrue, "", "", 3),
	)
	c, _ := newTestClient(t, handler)

	got, err := c.Wait(context.Background(), "my-svc", fastWait(3))
	if err != nil {
		t.Fatalf("Wait() error = %v", err)
	}
	if got == nil || got.Ready() == nil || got.Ready().Status != conditionTrue {
		t.Errorf("Wait() = %+v, want a ready status", got)
	}
	if calls() != 3 {
		t.Errorf("polled %d times, want 3", calls())
	}
}

func TestWaitFailsFastWhenReadyIsFalse(t *testing.T) {
	handler, calls := sequenceHandler(
		serviceWithReady(conditionFalse, "ConflictingRevisionName", "name taken", 3),
	)
	c, _ := newTestClient(t, handler)

	got, err := c.Wait(context.Background(), "my-svc", fastWait(3))
	if err == nil {
		t.Fatal("Wait() error = nil, want a rollout failure")
	}
	if !strings.Contains(err.Error(), "ConflictingRevisionName") || !strings.Contains(err.Error(), "name taken") {
		t.Errorf("Wait() error = %v, want the reason and message", err)
	}
	// 失敗が確定したら待ち続けない。
	if calls() != 1 {
		t.Errorf("polled %d times, want 1 (must not keep waiting after a failure)", calls())
	}
	if got == nil {
		t.Error("Wait() should return the last observed status even on failure")
	}
}

func TestWaitIgnoresThePreviousGeneration(t *testing.T) {
	// 直前の世代が Ready=True のまま残っている状態から始める。世代が追いつくまでは
	// 完了と判定してはいけない。
	handler, calls := sequenceHandler(
		serviceWithReady(conditionTrue, "", "", 2),
		serviceWithReady(conditionTrue, "", "", 2),
		serviceWithReady(conditionTrue, "", "", 3),
	)
	c, _ := newTestClient(t, handler)

	if _, err := c.Wait(context.Background(), "my-svc", fastWait(3)); err != nil {
		t.Fatalf("Wait() error = %v", err)
	}
	if calls() != 3 {
		t.Errorf("polled %d times, want 3 (must wait for observedGeneration to catch up)", calls())
	}
}

func TestWaitTimesOut(t *testing.T) {
	handler, _ := sequenceHandler(serviceWithReady("Unknown", "Deploying", "", 3))
	c, _ := newTestClient(t, handler)

	_, err := c.Wait(context.Background(), "my-svc", WaitOptions{
		Timeout: 50 * time.Millisecond, Interval: time.Millisecond, Generation: 3,
	})
	if err == nil {
		t.Fatal("Wait() error = nil, want a timeout")
	}
	for _, want := range []string{"timed out after", `"my-svc"`, "last seen", "Ready=Unknown"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Wait() error = %v, want it to contain %q", err, want)
		}
	}
}

func TestWaitStopsWhenTheContextIsCancelled(t *testing.T) {
	handler, _ := sequenceHandler(serviceWithReady("Unknown", "Deploying", "", 3))
	c, _ := newTestClient(t, handler)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	done := make(chan error, 1)
	go func() {
		_, err := c.Wait(ctx, "my-svc", WaitOptions{Timeout: time.Minute, Interval: time.Millisecond, Generation: 3})
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Wait() error = nil, want an interruption")
		}
		if !strings.Contains(err.Error(), "interrupted") || !errors.Is(err, context.Canceled) {
			t.Errorf("Wait() error = %v, want it to wrap context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Wait() did not return after the context was cancelled")
	}
}

func TestWaitReportsChangesOnce(t *testing.T) {
	handler, _ := sequenceHandler(
		serviceWithReady("Unknown", "Deploying", "", 3),
		serviceWithReady("Unknown", "Deploying", "", 3), // 同じ状態: 通知しない
		serviceWithReady(conditionTrue, "", "", 3),
	)
	c, _ := newTestClient(t, handler)

	var updates []string
	opts := fastWait(3)
	opts.OnUpdate = func(message string) { updates = append(updates, message) }
	if _, err := c.Wait(context.Background(), "my-svc", opts); err != nil {
		t.Fatalf("Wait() error = %v", err)
	}

	if len(updates) != 2 {
		t.Fatalf("OnUpdate called %d times (%v), want 2 (once per change)", len(updates), updates)
	}
	if !strings.Contains(updates[0], "Ready=Unknown") || !strings.Contains(updates[1], "Ready=True") {
		t.Errorf("updates = %v", updates)
	}
}

func TestWaitProgress(t *testing.T) {
	tests := []struct {
		name       string
		status     *Status
		generation int64
		want       string
	}{
		{name: "nil", status: nil, want: "unknown"},
		{
			name:   "no condition",
			status: newStatus(serviceWithReady("", "", "", 2)),
			want:   "observed generation 2, no Ready condition yet",
		},
		{
			name:   "ready",
			status: newStatus(serviceWithReady(conditionTrue, "", "", 3)),
			want:   "observed generation 3, Ready=True",
		},
		{
			name:   "failed",
			status: newStatus(serviceWithReady(conditionFalse, "RevisionFailed", "boom", 3)),
			want:   "observed generation 3, Ready=False (RevisionFailed): boom",
		},
		{
			// 待っている世代が未反映のうちは、前の世代の Ready を出さない。
			name:       "awaited generation not observed yet",
			status:     newStatus(serviceWithReady(conditionTrue, "", "", 7)),
			generation: 8,
			want:       "generation 8 not observed yet (currently 7)",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := waitProgress(tt.status, tt.generation); got != tt.want {
				t.Errorf("waitProgress() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestWaitToleratesTransientErrors は、状態の取得が一時的に失敗しても待機を
// 打ち切らないことを確認する。打ち切ると、適用は成功しているのに deploy が
// 失敗を返し、この待機が防ぎたい CI の誤判定を裏返しに作ってしまう。
func TestWaitToleratesTransientErrors(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	c, _ := newTestClient(t, func(*http.Request) (int, interface{}) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if calls <= 2 {
			return http.StatusServiceUnavailable, googleAPIError(503, "backend error")
		}
		return http.StatusOK, serviceWithReady(conditionTrue, "", "", 3)
	})

	var retries []error
	opts := fastWait(3)
	opts.OnRetry = func(err error) { retries = append(retries, err) }

	got, err := c.Wait(context.Background(), "my-svc", opts)
	if err != nil {
		t.Fatalf("Wait() error = %v, want the transient failures to be retried", err)
	}
	if got == nil || got.Ready() == nil || got.Ready().Status != conditionTrue {
		t.Errorf("Wait() = %+v, want a ready status", got)
	}
	if len(retries) != 2 {
		t.Errorf("OnRetry called %d times, want 2", len(retries))
	}
}

// TestWaitFailsFastWhenTheServiceIsMissing は 404 では再試行しないことを確認する。
// 実在しないサービスはタイムアウトまで待っても現れない。
func TestWaitFailsFastWhenTheServiceIsMissing(t *testing.T) {
	c, api := newTestClient(t, nil) // 既定の handler は 404

	_, err := c.Wait(context.Background(), "missing", fastWait(0))
	if err == nil {
		t.Fatal("Wait() error = nil, want a not-found error")
	}
	if !isNotFound(err) {
		t.Errorf("Wait() error = %v, want a 404", err)
	}
	if n := len(api.recorded()); n != 1 {
		t.Errorf("polled %d times, want 1 (a missing service must not be retried)", n)
	}
}

// TestWaitTimeoutReportsTheLastError は、取得に失敗し続けたままタイムアウトした
// 場合に原因を隠さないことを確認する。
func TestWaitTimeoutReportsTheLastError(t *testing.T) {
	c, _ := newTestClient(t, func(*http.Request) (int, interface{}) {
		return http.StatusServiceUnavailable, googleAPIError(503, "backend error")
	})

	_, err := c.Wait(context.Background(), "my-svc", WaitOptions{
		Timeout: 50 * time.Millisecond, Interval: time.Millisecond, Generation: 3,
	})
	if err == nil {
		t.Fatal("Wait() error = nil, want a timeout")
	}
	for _, want := range []string{"timed out after", "the last poll failed", "backend error"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Wait() error = %v, want it to contain %q", err, want)
		}
	}
}
