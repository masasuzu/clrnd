package cloudrun

import (
	"context"
	"fmt"
	"strings"

	run "google.golang.org/api/run/v1"
)

// conditionReady は「サービス全体が使える状態か」を表す条件の型名。
const conditionReady = "Ready"

// statusLabelWidth は Text() のラベル列の幅 (最長の "Latest created:" に合わせる)。
const statusLabelWidth = 17

// Status はサービスの現在状態。API のレスポンスから読み取り専用の情報だけを取り出した
// もので、JSON 出力の構造でもある。
type Status struct {
	Service               string          `json:"service"`
	URL                   string          `json:"url,omitempty"`
	LatestReadyRevision   string          `json:"latestReadyRevision,omitempty"`
	LatestCreatedRevision string          `json:"latestCreatedRevision,omitempty"`
	Generation            int64           `json:"generation,omitempty"`
	ObservedGeneration    int64           `json:"observedGeneration,omitempty"`
	Traffic               []TrafficTarget `json:"traffic,omitempty"`
	Conditions            []Condition     `json:"conditions,omitempty"`
}

// Condition は status.conditions の 1 件。
type Condition struct {
	Type               string `json:"type"`
	Status             string `json:"status"`
	Reason             string `json:"reason,omitempty"`
	Message            string `json:"message,omitempty"`
	LastTransitionTime string `json:"lastTransitionTime,omitempty"`
}

// TrafficTarget は status.traffic の 1 件 (実際に配分されているトラフィック)。
type TrafficTarget struct {
	RevisionName string `json:"revisionName,omitempty"`
	Tag          string `json:"tag,omitempty"`
	URL          string `json:"url,omitempty"`
	Percent      int64  `json:"percent"`
	Latest       bool   `json:"latestRevision,omitempty"`
}

// Status は指定したサービスの現在状態を取得する。読み取りのみで変更はしない。
func (c *Client) Status(ctx context.Context, service string) (*Status, error) {
	obj, err := c.GetService(ctx, service)
	if err != nil {
		return nil, err
	}
	return newStatus(obj), nil
}

// newStatus は API のレスポンスを Status に変換する。API アクセスを伴わない純粋な処理
// なので、整形の検証はこの関数だけで完結できる。
func newStatus(obj *run.Service) *Status {
	s := &Status{}
	if obj == nil {
		return s
	}
	if obj.Metadata != nil {
		s.Service = obj.Metadata.Name
		s.Generation = obj.Metadata.Generation
	}
	if obj.Status == nil {
		return s
	}

	s.URL = obj.Status.Url
	s.LatestReadyRevision = obj.Status.LatestReadyRevisionName
	s.LatestCreatedRevision = obj.Status.LatestCreatedRevisionName
	s.ObservedGeneration = obj.Status.ObservedGeneration

	for _, c := range obj.Status.Conditions {
		if c == nil {
			continue
		}
		s.Conditions = append(s.Conditions, Condition{
			Type:               c.Type,
			Status:             c.Status,
			Reason:             c.Reason,
			Message:            c.Message,
			LastTransitionTime: c.LastTransitionTime,
		})
	}
	for _, t := range obj.Status.Traffic {
		if t == nil {
			continue
		}
		s.Traffic = append(s.Traffic, TrafficTarget{
			RevisionName: t.RevisionName,
			Tag:          t.Tag,
			URL:          t.Url,
			Percent:      t.Percent,
			Latest:       t.LatestRevision,
		})
	}
	return s
}

// Ready は Ready 条件を返す。存在しなければ nil。wait (サービスの安定待ち) でも
// 同じ判定を使えるようにエクスポートしている。
func (s *Status) Ready() *Condition {
	for i := range s.Conditions {
		if s.Conditions[i].Type == conditionReady {
			return &s.Conditions[i]
		}
	}
	return nil
}

// Text は人間向けの整形出力を返す。末尾は改行で終わる。
func (s *Status) Text() string {
	var b strings.Builder
	line := func(label, value string) {
		if value == "" {
			return
		}
		fmt.Fprintf(&b, "%-*s%s\n", statusLabelWidth, label+":", value)
	}

	line("Service", s.Service)
	line("URL", s.URL)

	// Ready は理由があれば添える。メッセージは長いので独立した行に出す。
	if c := s.Ready(); c != nil {
		status := c.Status
		if c.Reason != "" {
			status = fmt.Sprintf("%s (%s)", c.Status, c.Reason)
		}
		line("Ready", status)
		line("Message", c.Message)
	}

	line("Latest ready", s.LatestReadyRevision)
	line("Latest created", s.LatestCreatedRevision)
	if s.Generation != 0 || s.ObservedGeneration != 0 {
		line("Generation", fmt.Sprintf("%d (observed %d)", s.Generation, s.ObservedGeneration))
	}

	if len(s.Traffic) > 0 {
		b.WriteString("Traffic:\n")
		for _, t := range s.Traffic {
			name := t.RevisionName
			if name == "" {
				// latestRevision 指定で、まだ解決前のリビジョンを指している場合。
				name = "(latest)"
			}
			tag := ""
			if t.Tag != "" {
				tag = fmt.Sprintf("  (tag: %s)", t.Tag)
			}
			fmt.Fprintf(&b, "  %3d%%  %s%s\n", t.Percent, name, tag)
		}
	}

	if len(s.Conditions) > 0 {
		b.WriteString("Conditions:\n")
		width := 0
		for _, c := range s.Conditions {
			if len(c.Type) > width {
				width = len(c.Type)
			}
		}
		for _, c := range s.Conditions {
			fmt.Fprintf(&b, "  %-*s  %s", width, c.Type, c.Status)
			if c.Reason != "" {
				fmt.Fprintf(&b, "  %s", c.Reason)
			}
			b.WriteString("\n")
		}
	}

	return b.String()
}
