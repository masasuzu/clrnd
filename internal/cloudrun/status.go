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

	// Ready is shown with its reason when there is one. The message is long, so it gets its own
	// line.
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
