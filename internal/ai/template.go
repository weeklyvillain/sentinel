package ai

import (
	"context"
	"fmt"

	"github.com/weeklyvillain/sentinel/internal/event"
)

// Template drafts issues without any model. It is the fallback when no
// provider is configured or the provider fails.
type Template struct{}

func (Template) Name() string { return "template" }

func (Template) Draft(_ context.Context, in Input) (Draft, error) {
	if in.Kind == event.KindDown {
		return Draft{
			Title:       fmt.Sprintf("[%s] service stopped sending telemetry", in.Service),
			Summary:     fmt.Sprintf("Sentinel has not received any logs, traces or metrics from %s for longer than the configured down threshold.", in.Service),
			LikelyCause: "The service (or its OpenTelemetry exporter/collector path) is down, crashed, or cannot reach Sentinel.",
			NextSteps: []string{
				"Check whether the service's processes/pods are running and healthy.",
				"Check the OpenTelemetry Collector and network path between the service and Sentinel.",
				"Look at recent deploys or infrastructure changes for this service.",
			},
		}, nil
	}
	var sample event.Event
	if len(in.Samples) > 0 {
		sample = in.Samples[len(in.Samples)-1]
	}
	what := sample.Summary()
	title := fmt.Sprintf("[%s] %s", in.Service, what)
	if len(title) > 100 {
		title = title[:97] + "..."
	}
	src := "log"
	if in.Kind == event.KindSpan {
		src = "span"
	}
	return Draft{
		Title:       title,
		Summary:     fmt.Sprintf("%s reported this error %d time(s) as %s events.", in.Service, in.Count, src),
		LikelyCause: "Not analyzed (no AI provider available). See the samples below.",
		NextSteps: []string{
			"Inspect the stack trace and sample events below.",
			"Use the trace IDs to look up the full request in your tracing backend.",
			"Check recent deploys of this service around the first-seen time.",
		},
	}, nil
}
