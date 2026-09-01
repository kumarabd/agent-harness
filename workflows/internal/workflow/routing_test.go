package workflow

import (
	"testing"

	"agent-harness/workflows/internal/types"
)

func TestRoute(t *testing.T) {
	cases := []struct {
		name string
		task types.TaskRepresentation
		want RoutingPlan
	}{
		{
			"high-confidence conversational fast-paths",
			types.TaskRepresentation{Intent: "conversational", Complexity: "trivial", Confidence: 0.95},
			RoutingPlan{FastPath: true},
		},
		{
			"meta gets memory only",
			types.TaskRepresentation{Intent: "meta", Complexity: "simple", Confidence: 0.9},
			RoutingPlan{Memory: true},
		},
		{
			"simple question gets memory only",
			types.TaskRepresentation{Intent: "question", Complexity: "simple", Confidence: 0.8},
			RoutingPlan{Memory: true},
		},
		{
			"complex question gets memory + skills",
			types.TaskRepresentation{Intent: "question", Complexity: "complex", Confidence: 0.8},
			RoutingPlan{Memory: true, Skills: true},
		},
		{
			"task gets the full path",
			types.TaskRepresentation{Intent: "task", Complexity: "moderate", Confidence: 0.8},
			RoutingPlan{Memory: true, Skills: true, Tools: true},
		},
		{
			"low confidence takes the full path regardless of intent",
			types.TaskRepresentation{Intent: "conversational", Complexity: "trivial", Confidence: 0.3},
			RoutingPlan{Memory: true, Skills: true, Tools: true},
		},
		{
			"zero confidence (step-2 fallback) takes the full path",
			types.TaskRepresentation{Intent: "task", Complexity: "moderate", Confidence: 0.0},
			RoutingPlan{Memory: true, Skills: true, Tools: true},
		},
		{
			"unknown intent takes the full path",
			types.TaskRepresentation{Intent: "??", Complexity: "moderate", Confidence: 0.9},
			RoutingPlan{Memory: true, Skills: true, Tools: true},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Route(c.task); got != c.want {
				t.Errorf("Route(%+v) = %+v, want %+v", c.task, got, c.want)
			}
		})
	}
}
