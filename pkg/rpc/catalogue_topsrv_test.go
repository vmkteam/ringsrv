package rpc

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/vmkteam/ringsrv/pkg/ring"
	"github.com/vmkteam/ringsrv/pkg/ring/target"
	"github.com/vmkteam/ringsrv/pkg/ring/target/targettest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The topsrv DefaultJQ takes the JSON-RPC envelope off and lifts an error
// out of an HTTP 200 (the shape Slack has too), folds the PromQL label
// pairs into the object the model knows from Prometheus, and trims alert
// rules to what an on-call reader compares a value against. Exercised on
// bodies shaped like the live answers of 2026-09-07, with neutral names,
// through the same gojq path the server uses, on every shipped catalogue.
func TestCatalogue_TopsrvDefaultJQ(t *testing.T) {
	t.Parallel()
	for _, c := range targettest.Catalogs(t) {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			cat, err := target.Load(c.Path)
			require.NoError(t, err, c.Source)
			p, ok := cat.Profile("topsrv")
			require.True(t, ok)
			require.NotEmpty(t, p.DefaultJQ)

			apply := func(t *testing.T, fixture string) any {
				t.Helper()
				body, err := os.ReadFile(filepath.Join("testdata", "topsrv", fixture))
				require.NoError(t, err)
				out, err := ring.ApplyJQ(context.Background(), p.DefaultJQ, body, time.Second)
				require.NoError(t, err, fixture)
				return out
			}

			t.Run("instant vector looks like Prometheus", func(t *testing.T) {
				out := apply(t, "query.json").(map[string]any)
				assert.Equal(t, "vector", out["resultType"])
				result := out["result"].([]any)
				require.Len(t, result, 1)
				series := result[0].(map[string]any)
				assert.Equal(t, map[string]any{"__name__": "topsrv_load_average", "instance": "db1", "interval": "1m"}, series["metric"], "label pairs became an object")
				assert.Equal(t, []any{1788790100, "0.28"}, series["value"], "one point is value, as in Prometheus")
				assert.NotContains(t, series, "values")
			})

			t.Run("matrix keeps every point", func(t *testing.T) {
				out := apply(t, "query_range.json").(map[string]any)
				assert.Equal(t, "matrix", out["resultType"])
				series := out["result"].([]any)[0].(map[string]any)
				assert.Equal(t, map[string]any{"instance": "db1"}, series["metric"])
				values := series["values"].([]any)
				require.Len(t, values, 3)
				assert.Equal(t, []any{1788786500, "0.49"}, values[0])
			})

			t.Run("no series is an empty result, not an error", func(t *testing.T) {
				out := apply(t, "empty.json").(map[string]any)
				assert.Equal(t, []any{}, out["result"])
			})

			t.Run("rules keep the expression and the threshold", func(t *testing.T) {
				rules := apply(t, "rules.json").([]any)
				require.Len(t, rules, 2)
				rule := rules[1].(map[string]any)
				assert.Equal(t, "http-502-504", rule["code"])
				assert.Contains(t, rule["promql"], "topsrv_nginx_http_requests_total")
				assert.Equal(t, ">", rule["comparator"])
				assert.InDelta(t, 0.005, rule["threshold"], 1e-9)
				assert.NotContains(t, rule, "repeatSeconds", "the scheduling of the rule is not what the reader compares against")
				assert.NotContains(t, rule, "isSystem")
				// An open event travels on the rule (topsrv since 2026-09-07);
				// a quiet rule carries neither key rather than two nulls.
				assert.Equal(t, "firing", rule["currentState"])
				assert.EqualValues(t, 149652, rule["activeEventId"])
				quiet := rules[0].(map[string]any)
				assert.NotContains(t, quiet, "currentState")
				assert.NotContains(t, quiet, "activeEventId")
			})

			t.Run("alert events fold their labels", func(t *testing.T) {
				events := apply(t, "alerts.json").([]any)
				require.Len(t, events, 2)
				second := events[1].(map[string]any)
				assert.Equal(t, map[string]any{"instance": "node1", "upstream": "api"}, second["labels"])
				assert.Equal(t, "resolved", second["state"])
				assert.Contains(t, second, "resolvedAt")
			})

			t.Run("hosts pass through without the envelope", func(t *testing.T) {
				hosts := apply(t, "hosts.json").([]any)
				require.Len(t, hosts, 2)
				db := hosts[0].(map[string]any)
				assert.Equal(t, "db1", db["hostname"])
				// addresses (topsrv since 2026-09-29) is how the model finds whose
				// machine an IP from a DSN is, so the filter must keep it whole.
				addrs := db["addresses"].([]any)
				require.Len(t, addrs, 2)
				assert.Equal(t, map[string]any{"address": "10.0.0.6", "interface": "bond0", "visibility": "private", "network": "10.0.0.0/24"}, addrs[0])
				assert.Equal(t, []any{}, hosts[1].(map[string]any)["addresses"], "a failed address lookup stays an empty list")
			})

			// The weblog answers (weblogs:read, 2026-09-09) are already flat:
			// the filter only has to take the envelope off and leave the
			// counters alone. A network row carries no promql and no
			// ruleCode, so the array must not be mistaken for alert rules
			// or alert events on its way out.
			t.Run("weblog groups keep their counters", func(t *testing.T) {
				out := apply(t, "weblog_top.json").(map[string]any)
				assert.Equal(t, "path", out["groupBy"])
				assert.EqualValues(t, 184, out["totalGroups"])
				row := out["rows"].([]any)[0].(map[string]any)
				assert.Equal(t, "/api/v1/search", row["value"])
				assert.EqualValues(t, 1153040, row["effortMs"], "effort is what the group costs the backend")
			})

			t.Run("weblog networks come out as a bare array", func(t *testing.T) {
				nets := apply(t, "weblog_networks.json").([]any)
				require.Len(t, nets, 2)
				first := nets[0].(map[string]any)
				assert.EqualValues(t, 64500, first["asn"])
				assert.Equal(t, "hosting", first["networkType"])
				assert.EqualValues(t, 3, first["uniqUAs"], "three User-Agents over 812 addresses is the pool signature")
				assert.Equal(t, []any{"Mozilla/5.0 (X11; Linux x86_64)"}, first["topUAs"])
			})

			t.Run("an error at HTTP 200 comes up top", func(t *testing.T) {
				out := apply(t, "error.json").(map[string]any)
				require.Contains(t, out, "error")
				e := out["error"].(map[string]any)
				assert.EqualValues(t, 400, e["code"])
				assert.Equal(t, "bad_query", e["message"])
				assert.Contains(t, e["data"], "labelFilterExpr", "the parser's message is what fixes the query")
				assert.NotContains(t, out, "result")
			})

			t.Run("a batch is shaped element by element", func(t *testing.T) {
				out := apply(t, "batch.json").([]any)
				require.Len(t, out, 2)
				assert.Equal(t, "acme", out[0].(map[string]any)["project"])
				assert.Contains(t, out[1].(map[string]any), "error")
			})
		})
	}
}
