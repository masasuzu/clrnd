package cloudrun

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	run "google.golang.org/api/run/v1"
)

// WorkerPoolStatus is the current state of a worker pool. It holds only read-only information
// taken from the API response, and it is also the structure of worker-pools status's JSON output.
type WorkerPoolStatus struct {
	WorkerPool string `json:"workerPool"`
	// ScalingMode and InstanceCount come from the worker pool's annotations. InstanceCount is nil
	// when the worker pool does not say, so that a count of 0 — a stopped worker pool — still shows.
	ScalingMode           string          `json:"scalingMode,omitempty"`
	InstanceCount         *int64          `json:"instanceCount,omitempty"`
	LatestReadyRevision   string          `json:"latestReadyRevision,omitempty"`
	LatestCreatedRevision string          `json:"latestCreatedRevision,omitempty"`
	Generation            int64           `json:"generation,omitempty"`
	ObservedGeneration    int64           `json:"observedGeneration,omitempty"`
	InstanceSplits        []InstanceSplit `json:"instanceSplits,omitempty"`
	Conditions            []Condition     `json:"conditions,omitempty"`
}

// InstanceSplit is a single entry of status.instanceSplits: the share of the instances a revision
// runs. The status always names the revision; Latest is kept for a split the API has not resolved.
type InstanceSplit struct {
	RevisionName string `json:"revisionName,omitempty"`
	Percent      int64  `json:"percent"`
	Latest       bool   `json:"latestRevision,omitempty"`
}

// GetWorkerPoolStatus fetches the current state of the worker pool. It only reads and changes
// nothing.
func (c *Client) GetWorkerPoolStatus(ctx context.Context, pool string) (*WorkerPoolStatus, error) {
	obj, err := workerPoolKind.get(ctx, c, pool)
	if err != nil {
		return nil, fmt.Errorf("failed to get worker pool %q: %w", pool, err)
	}
	return newWorkerPoolStatus(obj), nil
}

// newWorkerPoolStatus converts an API response into a WorkerPoolStatus. It is pure, so the
// formatting is testable without the API.
func newWorkerPoolStatus(obj *run.WorkerPool) *WorkerPoolStatus {
	s := &WorkerPoolStatus{}
	if obj == nil {
		return s
	}
	if meta := obj.Metadata; meta != nil {
		s.WorkerPool = meta.Name
		s.Generation = meta.Generation
		s.ScalingMode = strings.TrimSpace(meta.Annotations[annotationScalingMode])
		if v, ok := meta.Annotations[annotationManualInstanceCount]; ok {
			if n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil {
				s.InstanceCount = &n
			}
		}
	}
	if st := obj.Status; st != nil {
		s.LatestReadyRevision = st.LatestReadyRevisionName
		s.LatestCreatedRevision = st.LatestCreatedRevisionName
		s.ObservedGeneration = st.ObservedGeneration
		s.Conditions = newConditions(st.Conditions)
		for _, split := range st.InstanceSplits {
			if split == nil {
				continue
			}
			s.InstanceSplits = append(s.InstanceSplits, InstanceSplit{
				RevisionName: split.RevisionName,
				Percent:      split.Percent,
				Latest:       split.LatestRevision,
			})
		}
	}
	return s
}

// newWorkerPoolWaitStatus converts a worker pool into the part of a Status that waiting reads.
// Service holds the worker pool's name.
func newWorkerPoolWaitStatus(obj *run.WorkerPool) *Status {
	s := &Status{}
	if obj == nil {
		return s
	}
	if obj.Metadata != nil {
		s.Service = obj.Metadata.Name
		s.Generation = obj.Metadata.Generation
	}
	if st := obj.Status; st != nil {
		s.LatestReadyRevision = st.LatestReadyRevisionName
		s.LatestCreatedRevision = st.LatestCreatedRevisionName
		s.ObservedGeneration = st.ObservedGeneration
		s.Conditions = newConditions(st.Conditions)
	}
	return s
}

// Ready returns the Ready condition, or nil if there is none.
func (s *WorkerPoolStatus) Ready() *Condition { return readyCondition(s.Conditions) }

// Text returns the human-readable formatted output. It ends with a newline.
func (s *WorkerPoolStatus) Text() string {
	var b strings.Builder
	line := func(label, value string) {
		if value == "" {
			return
		}
		fmt.Fprintf(&b, "%-*s%s\n", statusLabelWidth, label+":", value)
	}

	line("Worker pool", s.WorkerPool)
	writeReady(line, s.Ready())
	line("Instances", s.instances())
	line("Latest ready", s.LatestReadyRevision)
	line("Latest created", s.LatestCreatedRevision)
	if s.Generation != 0 || s.ObservedGeneration != 0 {
		line("Generation", fmt.Sprintf("%d (observed %d)", s.Generation, s.ObservedGeneration))
	}

	if len(s.InstanceSplits) > 0 {
		b.WriteString("Instance split:\n")
		for _, split := range s.InstanceSplits {
			name := split.RevisionName
			if name == "" {
				name = "(latest)"
			}
			fmt.Fprintf(&b, "  %3d%%  %s\n", split.Percent, name)
		}
	}

	writeConditions(&b, s.Conditions)
	return b.String()
}

// instances describes the instance count for Text: the count with the scaling mode, and that the
// worker pool runs nothing at 0.
func (s *WorkerPoolStatus) instances() string {
	if s.InstanceCount == nil {
		return s.ScalingMode
	}
	detail := s.ScalingMode
	if *s.InstanceCount == 0 {
		detail = strings.TrimPrefix(detail+", stopped", ", ")
	}
	if detail == "" {
		return strconv.FormatInt(*s.InstanceCount, 10)
	}
	return fmt.Sprintf("%d (%s)", *s.InstanceCount, detail)
}
