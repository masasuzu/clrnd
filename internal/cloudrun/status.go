package cloudrun

import (
	"context"
	"fmt"
	"strings"

	run "google.golang.org/api/run/v1"
)

// conditionReady is the type name of the condition that says "the service as a whole is usable".
const conditionReady = "Ready"

// statusLabelWidth is the width of the label column in Text() (sized to the longest label,
// "Latest created:").
const statusLabelWidth = 17

// Status is the current state of a service. It holds only the read-only information taken from
// the API response, and it is also the structure of the JSON output.
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

// Condition is a single entry of status.conditions.
type Condition struct {
	Type               string `json:"type"`
	Status             string `json:"status"`
	Reason             string `json:"reason,omitempty"`
	Message            string `json:"message,omitempty"`
	LastTransitionTime string `json:"lastTransitionTime,omitempty"`
}

// TrafficTarget is a single entry of status.traffic (the traffic actually being routed).
type TrafficTarget struct {
	RevisionName string `json:"revisionName,omitempty"`
	Tag          string `json:"tag,omitempty"`
	URL          string `json:"url,omitempty"`
	Percent      int64  `json:"percent"`
	Latest       bool   `json:"latestRevision,omitempty"`
}

// Status fetches the current state of the given service. It only reads and changes nothing.
func (c *Client) Status(ctx context.Context, service string) (*Status, error) {
	obj, err := c.GetService(ctx, service)
	if err != nil {
		return nil, err
	}
	return newStatus(obj), nil
}

// newStatus converts an API response into a Status. It is pure, with no API access, so the
// formatting can be tested entirely through this function.
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
	s.Conditions = newConditions(obj.Status.Conditions)

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

// newJobStatus converts a job into the part of a Status that waiting reads: its generations and
// conditions. A job has no URL, revisions or traffic, so those stay empty, and Service holds the
// job's name.
func newJobStatus(obj *run.Job) *Status {
	s := &Status{}
	if obj == nil {
		return s
	}
	if obj.Metadata != nil {
		s.Service = obj.Metadata.Name
		s.Generation = obj.Metadata.Generation
	}
	if obj.Status != nil {
		s.ObservedGeneration = obj.Status.ObservedGeneration
		s.Conditions = newConditions(obj.Status.Conditions)
	}
	return s
}

// newConditions converts the API's conditions, skipping null entries.
func newConditions(in []*run.GoogleCloudRunV1Condition) []Condition {
	var out []Condition
	for _, c := range in {
		if c == nil {
			continue
		}
		out = append(out, Condition{
			Type:               c.Type,
			Status:             c.Status,
			Reason:             c.Reason,
			Message:            c.Message,
			LastTransitionTime: c.LastTransitionTime,
		})
	}
	return out
}

// Ready returns the Ready condition, or nil if there is none. It is exported so that wait
// (waiting for the service to settle) can use the same check.
func (s *Status) Ready() *Condition {
	for i := range s.Conditions {
		if s.Conditions[i].Type == conditionReady {
			return &s.Conditions[i]
		}
	}
	return nil
}

// Text returns the human-readable formatted output. It ends with a newline.
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

	writeReady(line, s.Ready())

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
				// Set via latestRevision and pointing at a revision not resolved yet.
				name = "(latest)"
			}
			tag := ""
			if t.Tag != "" {
				tag = fmt.Sprintf("  (tag: %s)", t.Tag)
			}
			fmt.Fprintf(&b, "  %3d%%  %s%s\n", t.Percent, name, tag)
		}
	}

	writeConditions(&b, s.Conditions)
	return b.String()
}

// writeReady writes the Ready condition through line: its status with the reason when there is
// one, and the message, which is long, on its own line. Nothing when there is no Ready condition.
func writeReady(line func(label, value string), c *Condition) {
	if c == nil {
		return
	}
	status := c.Status
	if c.Reason != "" {
		status = fmt.Sprintf("%s (%s)", c.Status, c.Reason)
	}
	line("Ready", status)
	line("Message", c.Message)
}

// writeConditions writes every condition as an aligned table. Nothing when there are none.
func writeConditions(b *strings.Builder, conds []Condition) {
	if len(conds) == 0 {
		return
	}
	b.WriteString("Conditions:\n")
	width := 0
	for _, c := range conds {
		if len(c.Type) > width {
			width = len(c.Type)
		}
	}
	for _, c := range conds {
		fmt.Fprintf(b, "  %-*s  %s", width, c.Type, c.Status)
		if c.Reason != "" {
			fmt.Fprintf(b, "  %s", c.Reason)
		}
		b.WriteString("\n")
	}
}
