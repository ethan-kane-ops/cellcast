package placement

import (
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	cellcastv1alpha1 "github.com/ethan-kane-ops/cellcast/api/v1alpha1"
)

func TestEachStageOfADecisionIsTimed(t *testing.T) {
	// Selecting the policy, filtering the fleet and scoring what is left are
	// each a span under the caller's, so a slow decision says which of the
	// three it was.
	engine := newEngine(t, loaded(t, map[string]float64{"c-a": 0.5}),
		cell("c-a", cellcastv1alpha1.ClusterStateLive, map[string]string{"env": "dev"}),
		policy("p", []cellcastv1alpha1.SubjectSelector{subject(nil)}, map[string]string{"env": "dev"}),
	)

	sr := tracetest.NewSpanRecorder()
	ctx, parent := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr)).Tracer("test").Start(t.Context(), "place")
	if _, err := engine.Place(ctx, caller(nil), Request{}); err != nil {
		t.Fatalf("Place() = %v, want a placement", err)
	}
	parent.End()

	stages := map[string]bool{}
	for _, s := range sr.Ended() {
		if s.Name() == "place" {
			continue
		}
		stages[s.Name()] = true
		if s.Parent().SpanID() != parent.SpanContext().SpanID() {
			t.Errorf("the %q span is not a child of the caller's span", s.Name())
		}
		if len(s.Attributes()) != 0 {
			t.Errorf("the %q span carries %v; the engine times its stages and the hub decides what a span says", s.Name(), s.Attributes())
		}
	}
	for _, want := range []string{"select policy", "filter", "score"} {
		if !stages[want] {
			t.Errorf("no %q span; recorded %v", want, stages)
		}
	}
}

func TestAnUntracedDecisionStartsNoSpans(t *testing.T) {
	// With tracing off there is no span in the context, and the stages must not
	// go looking for a global provider to start one from.
	engine := newEngine(t, loaded(t, map[string]float64{"c-a": 0.5}),
		cell("c-a", cellcastv1alpha1.ClusterStateLive, map[string]string{"env": "dev"}),
		policy("p", []cellcastv1alpha1.SubjectSelector{subject(nil)}, map[string]string{"env": "dev"}),
	)
	ctx, span := startSpan(t.Context(), "select policy")
	defer span.End()

	if span.IsRecording() {
		t.Error("a stage span records with no traced parent")
	}
	if _, err := engine.Place(ctx, caller(nil), Request{}); err != nil {
		t.Fatalf("Place() = %v, want a placement", err)
	}
}
