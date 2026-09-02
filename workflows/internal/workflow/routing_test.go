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
			"simple question gets memory only (Lite)",
			types.TaskRepresentation{Intent: "question", Complexity: "simple", Confidence: 0.8},
			RoutingPlan{Memory: true},
		},
		{
			"moderate question gets memory only (Lite)",
			types.TaskRepresentation{Intent: "question", Complexity: "moderate", Confidence: 0.8},
			RoutingPlan{Memory: true},
		},
		{
			"complex question takes the full path (Deliberate)",
			types.TaskRepresentation{Intent: "question", Complexity: "complex", Confidence: 0.8},
			RoutingPlan{Memory: true, Skills: true, Tools: true},
		},
		{
			"simple task gets memory only (Lite)",
			types.TaskRepresentation{Intent: "task", Complexity: "simple", Confidence: 0.9},
			RoutingPlan{Memory: true},
		},
		{
			"trivial task gets memory only (Lite)",
			types.TaskRepresentation{Intent: "task", Complexity: "trivial", Confidence: 0.9},
			RoutingPlan{Memory: true},
		},
		{
			"moderate task takes the full path (Deliberate)",
			types.TaskRepresentation{Intent: "task", Complexity: "moderate", Confidence: 0.8},
			RoutingPlan{Memory: true, Skills: true, Tools: true},
		},
		{
			"low confidence takes the full path regardless of intent",
			types.TaskRepresentation{Intent: "conversational", Complexity: "trivial", Confidence: 0.3},
			RoutingPlan{Memory: true, Skills: true, Tools: true},
		},
		{
			"very low confidence takes the full path",
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

func TestLaneIsDeliberate(t *testing.T) {
	// docs/components/lane-model.md — Deliberate is exactly (task, moderate|complex),
	// (question, complex), plus confidence < 0.5 and any unknown intent.
	cases := []struct {
		task types.TaskRepresentation
		want bool
	}{
		{types.TaskRepresentation{Intent: "task", Complexity: "moderate", Confidence: 0.9}, true},
		{types.TaskRepresentation{Intent: "task", Complexity: "complex", Confidence: 0.9}, true},
		{types.TaskRepresentation{Intent: "task", Complexity: "simple", Confidence: 0.9}, false},
		{types.TaskRepresentation{Intent: "task", Complexity: "trivial", Confidence: 0.9}, false},
		{types.TaskRepresentation{Intent: "question", Complexity: "complex", Confidence: 0.9}, true},
		{types.TaskRepresentation{Intent: "question", Complexity: "moderate", Confidence: 0.9}, false},
		{types.TaskRepresentation{Intent: "meta", Complexity: "complex", Confidence: 0.9}, false},
		{types.TaskRepresentation{Intent: "conversational", Complexity: "trivial", Confidence: 0.9}, false},
		{types.TaskRepresentation{Intent: "task", Complexity: "simple", Confidence: 0.3}, true}, // low-confidence override
		{types.TaskRepresentation{Intent: "??", Complexity: "simple", Confidence: 0.9}, true},   // unknown intent
	}
	for _, c := range cases {
		if got := laneIsDeliberate(c.task); got != c.want {
			t.Errorf("laneIsDeliberate(%+v) = %v, want %v", c.task, got, c.want)
		}
	}
}
