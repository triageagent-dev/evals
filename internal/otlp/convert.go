package otlp

import (
	tracepkg "github.com/triageagent-dev/agentevals-go/internal/trace"
)

const (
	otelScope        = "otel.scope.name"
	otelScopeVersion = "otel.scope.version"
	otelSchemaURL    = "otel.schema_url"
)

// ParseExportRequest converts an OTLP JSON export dict (resourceSpans, as
// produced natively or bridged from protobuf via DecodeProtobufTraces) into
// agentevals-go's normalized Trace/Span model, grouping spans by trace ID.
// Ported from loader/otlp.py's _parse_otlp_export / _build_traces.
func ParseExportRequest(body map[string]any) []*tracepkg.Trace {
	var allSpans []*tracepkg.Span

	for _, rs := range asSlice(body["resourceSpans"]) {
		resourceSpan := asMap(rs)
		resource := asMap(resourceSpan["resource"])
		resourceAttrs := decodeAttributes(asSlice(resource["attributes"]))
		resourceSchemaURL, _ := resourceSpan["schemaUrl"].(string)

		for _, ss := range asSlice(resourceSpan["scopeSpans"]) {
			scopeSpan := asMap(ss)
			scope := asMap(scopeSpan["scope"])
			scopeName, _ := scope["name"].(string)
			scopeVersion, _ := scope["version"].(string)
			schemaURL, _ := scopeSpan["schemaUrl"].(string)
			if schemaURL == "" {
				schemaURL = resourceSchemaURL
			}

			for _, sd := range asSlice(scopeSpan["spans"]) {
				spanData := asMap(sd)
				allSpans = append(allSpans, parseSpan(spanData, resourceAttrs, scopeName, scopeVersion, schemaURL))
			}
		}
	}

	return buildTraces(allSpans)
}

// parseSpan converts one OTLP JSON span dict to a normalized Span. Ported
// from loader/otlp.py's _parse_span. Resource attributes are merged in last
// and win on key collision, matching Python's attributes.update(resource_attrs).
func parseSpan(spanData map[string]any, resourceAttrs map[string]any, scopeName, scopeVersion, schemaURL string) *tracepkg.Span {
	attrs := decodeAttributes(asSlice(spanData["attributes"]))

	if scopeName != "" {
		attrs[otelScope] = scopeName
	}
	if scopeVersion != "" {
		attrs[otelScopeVersion] = scopeVersion
	}
	if schemaURL != "" {
		attrs[otelSchemaURL] = schemaURL
	}

	for k, v := range resourceAttrs {
		attrs[k] = v
	}

	startNs := decodeIntValue(spanData["startTimeUnixNano"])
	endNs := decodeIntValue(spanData["endTimeUnixNano"])
	startUs := startNs / 1000
	durationUs := (endNs - startNs) / 1000

	traceID, _ := spanData["traceId"].(string)
	spanID, _ := spanData["spanId"].(string)
	parentSpanID, _ := spanData["parentSpanId"].(string)
	name, _ := spanData["name"].(string)

	return &tracepkg.Span{
		TraceID:       traceID,
		SpanID:        spanID,
		ParentSpanID:  parentSpanID,
		OperationName: name,
		StartTime:     startUs,
		Duration:      durationUs,
		Tags:          attrs,
	}
}

// buildTraces groups a flat span list by trace ID and resolves parent/child
// pointers within each trace. Ported from loader/otlp.py's _build_traces
// (shared logic with the Jaeger loader's _parse_trace).
func buildTraces(spans []*tracepkg.Span) []*tracepkg.Trace {
	byTraceID := map[string][]*tracepkg.Span{}
	var order []string
	for _, s := range spans {
		if _, ok := byTraceID[s.TraceID]; !ok {
			order = append(order, s.TraceID)
		}
		byTraceID[s.TraceID] = append(byTraceID[s.TraceID], s)
	}

	traces := make([]*tracepkg.Trace, 0, len(byTraceID))
	for _, traceID := range order {
		traces = append(traces, BuildTrace(traceID, byTraceID[traceID]))
	}
	return traces
}

// BuildTrace resolves parent/child pointers across an arbitrary pool of
// spans and wraps them in a Trace, exported so session aggregation
// (internal/api) can rebuild a merged view across spans ingested from
// multiple OTLP batches/trace IDs sharing one session.
func BuildTrace(traceID string, spans []*tracepkg.Span) *tracepkg.Trace {
	spansByID := make(map[string]*tracepkg.Span, len(spans))
	for _, s := range spans {
		spansByID[s.SpanID] = s
		s.Children = nil // rebuilding from scratch each call
	}

	var roots []*tracepkg.Span
	for _, s := range spans {
		if s.ParentSpanID != "" {
			if parent, ok := spansByID[s.ParentSpanID]; ok {
				parent.Children = append(parent.Children, s)
				continue
			}
		}
		roots = append(roots, s)
	}

	for _, s := range spans {
		sortByStart(s.Children)
	}
	sortByStart(roots)

	return &tracepkg.Trace{TraceID: traceID, RootSpans: roots, AllSpans: spans}
}

func sortByStart(spans []*tracepkg.Span) {
	for i := 1; i < len(spans); i++ {
		for j := i; j > 0 && spans[j-1].StartTime > spans[j].StartTime; j-- {
			spans[j-1], spans[j] = spans[j], spans[j-1]
		}
	}
}
