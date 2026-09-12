package agent

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// tracedClient is testClient reporting its spans to the returned recorder.
func tracedClient(t *testing.T, endpoint, token string) (*HubClient, *tracetest.SpanRecorder) {
	t.Helper()
	sr := tracetest.NewSpanRecorder()
	client := testClient(t, endpoint, tokenFile(t, token))
	client.tracerProvider = sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	return client, sr
}

func TestAReportCarriesItsTraceToTheHub(t *testing.T) {
	// The hub's span for a report joins the agent's trace only if the agent
	// sends it. Without it, a slow heartbeat is two halves nobody can join.
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("traceparent")
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	client, sr := tracedClient(t, srv.URL, "projected-token")
	if err := client.Report(t.Context(), Snapshot{Nodes: 1}); err != nil {
		t.Fatalf("Report() = %v, want nil", err)
	}

	spans := sr.Ended()
	if len(spans) != 1 {
		t.Fatalf("recorded %d spans, want the one report", len(spans))
	}
	span := spans[0]
	if span.Name() != reportSpanName || span.SpanKind() != trace.SpanKindClient {
		t.Errorf("span = %q (%s), want %q as a client span", span.Name(), span.SpanKind(), reportSpanName)
	}
	sc := span.SpanContext()
	if want := "00-" + sc.TraceID().String() + "-" + sc.SpanID().String() + "-01"; got != want {
		t.Errorf("traceparent = %q, want %q", got, want)
	}
	if span.Status().Code == codes.Error {
		t.Error("an accepted report was marked as failed")
	}
}

func TestARefusedReportMarksItsSpanAndCarriesNoToken(t *testing.T) {
	const token = "SENTINEL-AGENT-TOKEN-51c2e9"
	const hubSays = "cell prod-euw1 is not registered to this reporter"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"` + hubSays + `"}`))
	}))
	defer srv.Close()

	client, sr := tracedClient(t, srv.URL, token)
	var refused *HubError
	if err := client.Report(t.Context(), Snapshot{Nodes: 1}); !errors.As(err, &refused) {
		t.Fatalf("Report() = %v, want a HubError", err)
	}

	span := sr.Ended()[0]
	if span.Status().Code != codes.Error {
		t.Errorf("a refused report left its span %v, want an error", span.Status().Code)
	}
	var status string
	text := span.Name() + " " + span.Status().Description
	for _, kv := range span.Attributes() {
		text += " " + string(kv.Key) + "=" + kv.Value.String()
		if kv.Key == "http.response.status_code" {
			status = kv.Value.String()
		}
	}
	if status != "403" {
		t.Errorf("http.response.status_code = %q, want 403", status)
	}
	// The request carried the agent's token and the response the hub's words.
	// The span says that a report was refused and with what status, and no more.
	for _, leak := range []string{token, hubSays} {
		if strings.Contains(text, leak) {
			t.Errorf("the span carries %q:\n%s", leak, text)
		}
	}
}
