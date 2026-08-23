package cloudrun

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// 条件の Status がとる値。Unknown はまだ収束していないことを表す。
const (
	conditionTrue  = "True"
	conditionFalse = "False"
)

// 待機のパラメータの既定値。
const (
	defaultWaitTimeout  = 10 * time.Minute
	defaultWaitInterval = 2 * time.Second
	// ポーリング間隔の上限。長い起動を待つ間に API を叩き続けないよう、
	// 間隔を少しずつ伸ばして頭打ちにする。
	maxWaitInterval = 15 * time.Second
	waitBackoffNum  = 3
	waitBackoffDen  = 2
)

// WaitOptions は Wait の挙動を決める。ゼロ値でも使える (既定値が入る)。
type WaitOptions struct {
	// Timeout は待機全体の上限。0 なら 10 分。
	Timeout time.Duration
	// Interval は最初のポーリング間隔。0 なら 2 秒。以降 15 秒まで伸びる。
	Interval time.Duration
	// Generation は「この世代以上が反映されるまで待つ」指定。deploy 直後に、
	// 自分が適用した世代のロールアウトだけを見るために使う。0 なら世代は問わない。
	Generation int64
	// OnUpdate は状態が変わったときに、表示用の 1 行を伴って呼ばれる
	// (最初の取得時にも呼ばれる)。nil なら何もしない。
	OnUpdate func(message string)
	// OnRetry は状態の取得に失敗して再試行するときに呼ばれる。nil なら何もしない。
	OnRetry func(err error)
}

// Wait はサービスが安定するまでポーリングする。Ready=True になれば成功、
// Ready=False になった時点で失敗として返す (無駄に待たない)。
// ctx が cancel されると即座に戻るので、Ctrl-C で中断できる。
//
// 戻り値の *Status は最後に観測した状態で、エラー時も (取得できていれば) 返す。
func (c *Client) Wait(ctx context.Context, service string, opts WaitOptions) (*Status, error) {
	timeout, interval := waitDefaults(opts)
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var last *Status
	var previous string
	// lastErr は「観測に失敗したが再試行で回復するかもしれない」エラー。
	// これで待機を打ち切ると、適用自体は成功しているのに deploy が失敗を返す。
	// それは本来この待機が防ぎたい CI の誤判定を裏返しに作ってしまう。
	var lastErr error
	for {
		status, err := c.Status(waitCtx, service)
		switch {
		case err == nil:
			lastErr = nil
			last = status

			if progress := waitProgress(status, opts.Generation); progress != previous {
				previous = progress
				if opts.OnUpdate != nil {
					opts.OnUpdate(progress)
				}
			}
			if done, doneErr := waitDone(status, service, opts.Generation); done {
				return status, doneErr
			}

		case waitCtx.Err() != nil:
			// 待機中の cancel/期限切れは、API のエラーではなく待機の結果として返す。
			return last, waitInterrupted(ctx, waitCtx, service, last, timeout, opts.Generation, lastErr)

		case !isRetryable(err):
			// 待っても回復しない失敗。実在しないサービス (404) は現れないし、
			// 不正な要求 (400) や認証・権限の不備 (401/403) も時間では変わらない。
			return last, err

		default:
			// 一時的な失敗 (503/429/接続断など)。タイムアウトまで再試行する。
			lastErr = err
			if opts.OnRetry != nil {
				opts.OnRetry(err)
			}
		}

		select {
		case <-waitCtx.Done():
			return last, waitInterrupted(ctx, waitCtx, service, last, timeout, opts.Generation, lastErr)
		case <-time.After(interval):
		}

		interval = nextWaitInterval(interval)
	}
}

// WaitDeleted はサービスが実際に消えるまで待つ。Cloud Run の削除は非同期で、
// DELETE が受理された時点ではまだ取得できてしまうため、これが無いと
// 「削除してから作り直す」ような手順が競合する。
func (c *Client) WaitDeleted(ctx context.Context, service string, opts WaitOptions) error {
	timeout, interval := waitDefaults(opts)
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var lastErr error
	notified := false
	for {
		_, err := c.GetService(waitCtx, service)
		switch {
		case isNotFound(err):
			// 消えた。これが待っていた結果。
			return nil
		case err == nil:
			lastErr = nil
			if !notified {
				notified = true
				if opts.OnUpdate != nil {
					opts.OnUpdate("still present")
				}
			}
		case waitCtx.Err() != nil:
			return waitDeleteInterrupted(ctx, waitCtx, service, timeout, lastErr)
		case !isRetryable(err):
			// Wait と同じ分類。待っても回復しない失敗は、その場で返す。
			return err
		default:
			// 一時的な失敗の可能性がある。Wait と同じくタイムアウトまで再試行する。
			lastErr = err
			if opts.OnRetry != nil {
				opts.OnRetry(err)
			}
		}

		select {
		case <-waitCtx.Done():
			return waitDeleteInterrupted(ctx, waitCtx, service, timeout, lastErr)
		case <-time.After(interval):
		}
		interval = nextWaitInterval(interval)
	}
}

// waitDeleteInterrupted は削除待ちが打ち切られた理由を組み立てる。
func waitDeleteInterrupted(parent, waitCtx context.Context, service string,
	timeout time.Duration, lastErr error) error {
	if parent.Err() != nil {
		return fmt.Errorf("interrupted while waiting for service %q to be deleted: %w", service, parent.Err())
	}
	if errors.Is(waitCtx.Err(), context.DeadlineExceeded) {
		if lastErr != nil {
			return fmt.Errorf("timed out after %s waiting for service %q to be deleted; the last poll failed: %w",
				timeout, service, lastErr)
		}
		return fmt.Errorf("timed out after %s waiting for service %q to be deleted", timeout, service)
	}
	return fmt.Errorf("stopped waiting for service %q to be deleted: %w", service, waitCtx.Err())
}

// waitDefaults は未指定のパラメータを既定値で埋める。
func waitDefaults(opts WaitOptions) (timeout, interval time.Duration) {
	timeout, interval = opts.Timeout, opts.Interval
	if timeout <= 0 {
		timeout = defaultWaitTimeout
	}
	if interval <= 0 {
		interval = defaultWaitInterval
	}
	return timeout, interval
}

// nextWaitInterval は次のポーリング間隔を返す。上限まで少しずつ伸ばす。
// 利用者が上限より長い間隔を指定している場合はそれを尊重し、縮めない
// (--interval 60s は「API を叩く回数を減らしたい」という意思表示なので、
// それを 15s に切り下げるとかえって呼び出しを増やしてしまう)。
func nextWaitInterval(interval time.Duration) time.Duration {
	if interval >= maxWaitInterval {
		return interval
	}
	next := interval * waitBackoffNum / waitBackoffDen
	if next > maxWaitInterval {
		return maxWaitInterval
	}
	return next
}

// waitDone は現在の状態で待機を終えてよいかを返す。done が true でエラーが nil なら成功、
// エラー付きならロールアウトの失敗。done が false なら継続する。
//
// Cloud Run のドキュメントどおり、observedGeneration が対象の世代に追いつくまでは
// conditions が前の世代のものなので判定しない。
func waitDone(s *Status, service string, generation int64) (bool, error) {
	if s.ObservedGeneration < generation {
		return false, nil
	}
	ready := s.Ready()
	if ready == nil {
		return false, nil
	}
	switch ready.Status {
	case conditionTrue:
		return true, nil
	case conditionFalse:
		return true, fmt.Errorf("service %q failed to become ready: %s", service, conditionDetail(ready))
	}
	// Unknown: まだ収束していない。
	return false, nil
}

// waitInterrupted は待機が打ち切られた理由を組み立てる。呼び出し元の ctx が
// cancel されていれば中断 (Ctrl-C など)、そうでなければタイムアウト。
func waitInterrupted(parent, waitCtx context.Context, service string, last *Status,
	timeout time.Duration, generation int64, lastErr error) error {
	if parent.Err() != nil {
		return fmt.Errorf("interrupted while waiting for service %q: %w", service, parent.Err())
	}
	if errors.Is(waitCtx.Err(), context.DeadlineExceeded) {
		// 最後の取得が失敗したままなら、その原因を隠さない。
		if lastErr != nil {
			return fmt.Errorf("timed out after %s waiting for service %q; the last poll failed: %w",
				timeout, service, lastErr)
		}
		return fmt.Errorf("timed out after %s waiting for service %q to become ready (last seen: %s)",
			timeout, service, waitProgress(last, generation))
	}
	return fmt.Errorf("stopped waiting for service %q: %w", service, waitCtx.Err())
}

// waitProgress は進捗表示と状態比較に使う 1 行の状態表現を返す。nil でも安全。
// 待っている世代がまだ反映されていない間は Ready を出さない。そこで見える Ready は
// 前の世代のもので、表示すると「もう終わった」と誤読させるため。
func waitProgress(s *Status, generation int64) string {
	if s == nil {
		return "unknown"
	}
	if s.ObservedGeneration < generation {
		return fmt.Sprintf("generation %d not observed yet (currently %d)", generation, s.ObservedGeneration)
	}
	ready := s.Ready()
	if ready == nil {
		return fmt.Sprintf("observed generation %d, no Ready condition yet", s.ObservedGeneration)
	}
	return fmt.Sprintf("observed generation %d, Ready=%s", s.ObservedGeneration, conditionDetail(ready))
}

// conditionDetail は条件を "Status (Reason): Message" の形に整える。
func conditionDetail(c *Condition) string {
	detail := c.Status
	if c.Reason != "" {
		detail = fmt.Sprintf("%s (%s)", detail, c.Reason)
	}
	if c.Message != "" {
		detail = fmt.Sprintf("%s: %s", detail, c.Message)
	}
	return detail
}
