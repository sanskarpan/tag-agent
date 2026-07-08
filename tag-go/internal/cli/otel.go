package cli

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"
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
				started_at, COALESCE(finished_at,''), COALESCE(duration_ms,0), status, prompt_tokens, completion_tokens
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
				rows.Scan(&id, &tid, &pid, &name, &prof, &model, &start, &fin, &dur, &status, &pt, &ct)
				attrs := []map[string]any{
					otelAttr("gen_ai.system", model),
					otelAttr("gen_ai.request.model", model),
					otelAttr("tag.profile", prof),
					otelAttrInt("gen_ai.usage.input_tokens", pt),
					otelAttrInt("gen_ai.usage.output_tokens", ct),
				}
				spans = append(spans, map[string]any{
					"traceId": tid, "spanId": id, "parentSpanId": pid, "name": name,
					"startTimeUnixNano": start, "endTimeUnixNano": fin,
					"status":     map[string]any{"code": status},
					"attributes": attrs,
				})
			}
			payload := map[string]any{
				"resourceSpans": []map[string]any{{
					"resource":   map[string]any{"attributes": []map[string]any{otelAttr("service.name", "tag")}},
					"scopeSpans": []map[string]any{{"spans": spans}},
				}},
				"_semconv_version": "1.27.0",
				"_exported_spans":  len(spans),
			}
			b, _ := json.MarshalIndent(payload, "", "  ")
			fmt.Println(string(b))
			return nil
		},
	}
	c.Flags().StringVar(&traceID, "trace-id", "", "export only this trace")
	root.AddCommand(c)
}

func otelAttr(k, v string) map[string]any {
	return map[string]any{"key": k, "value": map[string]any{"stringValue": v}}
}
func otelAttrInt(k string, v int) map[string]any {
	return map[string]any{"key": k, "value": map[string]any{"intValue": v}}
}
