package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/tag-agent/tag/internal/version"
)

// registerOtelExport wires `otel-export` — export spans as OTLP/JSON with OTel
// GenAI semconv attributes. Port of cmd_otel_export (the --json/dry-run path;
// live OTLP POST to a collector endpoint is out of scope here).
func registerOtelExport(root *cobra.Command, app *App) {
	var traceID string
	c := &cobra.Command{
		Use:     "otel-export",
		Short:   "Export spans as OTLP/JSON (OTel GenAI semconv)",
		GroupID: "obs",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := app.OpenDB()
			if err != nil {
				return err
			}
			q := `SELECT id, trace_id, COALESCE(parent_id,''), name, COALESCE(profile,''), COALESCE(model_id,''),
				started_at, COALESCE(finished_at,''), COALESCE(duration_ms,0), status, prompt_tokens, completion_tokens, cost_usd
				FROM spans`
			var qargs []any
			if traceID != "" {
				q += ` WHERE trace_id=? ORDER BY started_at`
				qargs = append(qargs, traceID)
			} else {
				q += ` ORDER BY started_at DESC LIMIT 100`
			}
			rows, err := db.Query(q, qargs...)
			if err != nil {
				return err
			}
			defer rows.Close()
			var spans []map[string]any
			for rows.Next() {
				var id, tid, pid, name, prof, model, start, fin, status string
				var dur, pt, ct int
				var stored *float64
				if err := rows.Scan(&id, &tid, &pid, &name, &prof, &model, &start, &fin, &dur, &status, &pt, &ct, &stored); err != nil {
					return err
				}
				attrs := []map[string]any{
					otelAttr("gen_ai.system", otelDetectProvider(model)),
					otelAttr("gen_ai.request.model", model),
					otelAttr("tag.profile", prof),
					otelAttrInt("gen_ai.usage.input_tokens", pt),
					otelAttrInt("gen_ai.usage.output_tokens", ct),
				}
				attrs = otelAppendCost(attrs, model, stored, pt, ct)
				spans = append(spans, otelSpanJSON(id, tid, pid, name, start, fin, status, attrs))
			}
			if err := rows.Err(); err != nil {
				return err
			}
			// A --trace-id matching nothing must not emit a valid-looking empty
			// OTLP envelope with exit 0 — a typo'd id would read as a successful
			// export of nothing. Error like `costs --run-id bogus` (#763).
			if traceID != "" && len(spans) == 0 {
				return jsonErrorMaybe(fmt.Errorf("no spans recorded for trace %q", traceID))
			}
			payload := otelPayload(spans)
			b, _ := json.MarshalIndent(payload, "", "  ")
			fmt.Println(string(b))
			return nil
		},
	}
	c.Flags().StringVar(&traceID, "trace-id", "", "export only this trace")
	root.AddCommand(c)
}

// otelSemconvVersion is the pinned OTel GenAI semconv version (mirrors
// otel_semconv.SEMCONV_VERSION). It is reported as a scope attribute — never as
// a top-level key, which strict OTLP collectors reject.
const otelSemconvVersion = "1.28.0"

// otelSpanJSON builds one OTLP/JSON span. Ids are normalized to OTLP's required
// widths and status is the numeric StatusCode enum (port of
// otel_semconv.spans_to_otlp_json).
func otelSpanJSON(id, traceID, parentID, name, start, fin, status string, attrs []map[string]any) map[string]any {
	span := map[string]any{
		"traceId":           otelHexID(traceID, 32),
		"spanId":            otelHexID(id, 16),
		"name":              name,
		"kind":              3, // CLIENT
		"startTimeUnixNano": otelISOToNanos(start),
		"endTimeUnixNano":   otelISOToNanos(fin),
		"attributes":        attrs,
		"status":            map[string]any{"code": otelStatusCode(status)},
	}
	// An empty parentSpanId is invalid OTLP; the field is simply omitted for
	// root spans (same as the Python exporter).
	if p := strings.ReplaceAll(parentID, "-", ""); p != "" {
		span["parentSpanId"] = otelHexID(parentID, 16)
	}
	return span
}

// otelPayload wraps spans in the OTLP resourceSpans envelope. No underscore
// -prefixed bookkeeping keys are emitted: unknown top-level fields are rejected
// by strict collectors.
func otelPayload(spans []map[string]any) map[string]any {
	if spans == nil {
		spans = []map[string]any{}
	}
	return map[string]any{
		"resourceSpans": []map[string]any{{
			"resource": map[string]any{"attributes": []map[string]any{otelAttr("service.name", "tag-agent")}},
			"scopeSpans": []map[string]any{{
				"scope": map[string]any{
					"name":    "tag-agent",
					"version": version.Version,
					"attributes": []map[string]any{
						otelAttr("otel.semconv.version", otelSemconvVersion),
						otelAttr("otel.semconv.stability", "Development"),
					},
				},
				"spans": spans,
			}},
		}},
	}
}

// otelStatusCode maps a TAG span status to an OTLP StatusCode enum value
// (UNSET=0, OK=1, ERROR=2). Port of otel_semconv._otlp_status_code: a missing
// status maps to UNSET, not ERROR, so status-less spans do not inflate backend
// error rates.
func otelStatusCode(status string) int {
	switch status {
	case "ok":
		return 1
	case "", "unset":
		return 0
	default:
		return 2
	}
}

// otelHexID normalizes a TAG id into an OTLP-valid lowercase-hex id of exactly
// n chars. TAG ids are uuid hex, so the dash-strip / truncate / left-zero-pad
// path is byte-identical to otel_semconv's
// `(id or uuid4().hex).replace("-", "")[:n].zfill(n)`. Ids that are not hex
// (synthetic or legacy ids) would make the document invalid, so they are folded
// deterministically through SHA-256 instead of being emitted verbatim.
func otelHexID(id string, n int) string {
	s := strings.ToLower(strings.ReplaceAll(id, "-", ""))
	if len(s) > n {
		s = s[:n]
	}
	if s != "" && otelIsHex(s) {
		return strings.Repeat("0", n-len(s)) + s
	}
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:])[:n]
}

func otelIsHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func otelAttr(k, v string) map[string]any {
	return map[string]any{"key": k, "value": map[string]any{"stringValue": v}}
}
func otelAttrInt(k string, v int) map[string]any {
	return map[string]any{"key": k, "value": map[string]any{"intValue": v}}
}
func otelAttrDouble(k string, v float64) map[string]any {
	return map[string]any{"key": k, "value": map[string]any{"doubleValue": v}}
}

// otelAppendCost adds the per-span cost attributes of PRD-046 §10.7 / PRD-041.
//
// The attribute is OMITTED entirely when no rate is known for the model — a
// literal 0.0 would tell a backend the turn was free, which is a different claim
// from "TAG cannot price this model". When the rate behind the number is an
// estimate, `tag.cost.rate_estimated=true` rides along so a dashboard summing
// gen_ai.usage.cost_usd can tell corroborated dollars from guessed ones.
func otelAppendCost(attrs []map[string]any, model string, stored *float64, promptTokens, completionTokens int) []map[string]any {
	cost, estimated, _ := resolveSpanCost(model, stored, promptTokens, completionTokens)
	if cost == nil {
		return attrs
	}
	attrs = append(attrs, otelAttrDouble("gen_ai.usage.cost_usd", *cost))
	if estimated {
		attrs = append(attrs, map[string]any{
			"key": "tag.cost.rate_estimated", "value": map[string]any{"boolValue": true}})
	}
	return attrs
}

// otelProviderNamespaces / otelProviderPrefixes mirror otel_semconv.py's
// _PROVIDER_NAMESPACES and _PROVIDER_MAP for gen_ai.system detection.
var otelProviderNamespaces = map[string]string{
	"anthropic": "anthropic", "openai": "openai", "google": "google",
	"mistral": "mistral", "meta": "meta", "meta-llama": "meta", "cohere": "cohere",
}

var otelProviderPrefixes = []struct{ prefix, provider string }{
	{"claude", "anthropic"}, {"gpt", "openai"}, {"gemini", "google"},
	{"mistral", "mistral"}, {"llama", "meta"}, {"command", "cohere"},
}

// otelDetectProvider infers gen_ai.system from a model id (port of
// otel_semconv.detect_provider): handles both bare names ("gpt-4o") and
// provider-namespaced ids ("anthropic/claude-sonnet-4-6").
func otelDetectProvider(modelID string) string {
	if modelID == "" {
		return "unknown"
	}
	m := strings.ToLower(modelID)
	if ns, rest, ok := strings.Cut(m, "/"); ok {
		if p, ok := otelProviderNamespaces[ns]; ok {
			return p
		}
		m = rest
	}
	for _, e := range otelProviderPrefixes {
		if strings.HasPrefix(m, e.prefix) {
			return e.provider
		}
	}
	return "unknown"
}

// otelISOToNanos converts an ISO timestamp to Unix nanoseconds as the
// stringified int64 OTLP/JSON expects (port of otel_semconv._iso_to_ns;
// unparseable/empty inputs map to "0").
func otelISOToNanos(iso string) string {
	if iso == "" {
		return "0"
	}
	t, err := time.Parse(time.RFC3339Nano, iso)
	if err != nil {
		t, err = time.ParseInLocation("2006-01-02T15:04:05", iso, time.UTC)
		if err != nil {
			return "0"
		}
	}
	return strconv.FormatInt(t.UnixNano(), 10)
}
