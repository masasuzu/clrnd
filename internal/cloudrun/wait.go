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
	// OnUpdate は状態が変わったときに呼ばれる (最初の取得時にも呼ばれる)。
	// 進捗表示に使う。nil なら何もしない。
	OnUpdate func(*Status)
}

// Wait はサービスが安定するまでポーリングする。Ready=True になれば成功、
// Ready=False になった時点で失敗として返す (無駄に待たない)。
// ctx が cancel されると即座に戻るので、Ctrl-C で中断できる。
//
// 戻り値の *Status は最後に観測した状態で、エラー時も (取得できていれば) 返す。
func (c *Client) Wait(ctx context.Context, service string, opts WaitOptions) (*Status, error) {
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = defaultWaitTimeout
	}
	interval := opts.Interval
	if interval <= 0 {
		interval = defaultWaitInterval
	}

	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var last *Status
	var previous string
	for {
		status, err := c.Status(waitCtx, service)
		if err != nil {
			// 待機中の cancel/期限切れは、API のエラーではなく待機の結果として返す。
			if waitCtx.Err() != nil {
				return last, waitInterrupted(ctx, waitCtx, service, last, timeout)
			}
			return last, err
		}
		last = status

		if summary := status.Summary(); summary != previous {
			previous = summary
			if opts.OnUpdate != nil {
				opts.OnUpdate(status)
			}
		}

		if done, err := waitDone(status, service, opts.Generation); done {
			return status, err
		}

		select {
		case <-waitCtx.Done():
			return last, waitInterrupted(ctx, waitCtx, service, last, timeout)
		case <-time.After(interval):
		}

		if interval < maxWaitInterval {
			interval = interval * waitBackoffNum / waitBackoffDen
			if interval > maxWaitInterval {
				interval = maxWaitInterval
			}
		}
	}
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
func waitInterrupted(parent, waitCtx context.Context, service string, last *Status, timeout time.Duration) error {
	if parent.Err() != nil {
		return fmt.Errorf("interrupted while waiting for service %q: %w", service, parent.Err())
	}
	if errors.Is(waitCtx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("timed out after %s waiting for service %q to become ready (last seen: %s)",
			timeout, service, last.Summary())
	}
	return fmt.Errorf("stopped waiting for service %q: %w", service, waitCtx.Err())
}

// Summary は進捗表示と状態比較に使う 1 行の状態表現を返す。nil でも安全。
func (s *Status) Summary() string {
	if s == nil {
		return "unknown"
	}
	ready := s.Ready()
	if ready == nil {
		return fmt.Sprintf("generation %d, no Ready condition yet", s.ObservedGeneration)
	}
	return fmt.Sprintf("generation %d, Ready=%s", s.ObservedGeneration, conditionDetail(ready))
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
