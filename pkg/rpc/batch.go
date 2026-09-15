package rpc

// Batching. Every tool that used to take one thing now takes a list and answers
// per item.
//
// The reason is measured: in one day's audit log, of 341 neighbouring call pairs
// inside a trace only 4 were less than 150 ms apart — the median gap is 2.8
// seconds. The client dispatches one call at a time and waits, so N independent
// questions cost N round trips; asking for parallel calls in prose produced
// those 4 pairs, and a list in the schema is the only place they can overlap.
//
// No tool keeps its scalar form beside the list: with both shapes advertised the
// model picks the simple one, and the batch never runs.

import (
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/sync/errgroup"
)

// Batch limits, one per tool, sized against what an item costs:
//
//   - api_call items are HTTP calls through one instance-wide semaphore, so the
//     list is long but runs a few at a time. Twenty covers the widest fan-out
//     seen in the log.
//   - db_query items each hold a connection and a statement_timeout, and the log
//     shows drill-down rather than fan-out.
//   - db_introspect, repo_map and help read schemas, the catalogue and the
//     compiled-in sheets: cheap, bounded by how many a step can use.
const (
	maxAPICallBatch     = 20
	maxDBQueryBatch     = 5
	maxIntrospectBatch  = 6
	maxRepoMapBatch     = 10
	maxHelpBatch        = 6
	defaultBatchWorkers = 4
)

// checkBatch validates the size of a batch argument that has to carry
// something.
func checkBatch(n, maxN int, field string) error {
	if n == 0 {
		// Naming the old scalar too: a caller still sending a single value
		// arrives here with an empty list.
		return fmt.Errorf("%s must not be empty — it takes a list (%q), not a single %s",
			field, field, strings.TrimSuffix(field, "s"))
	}
	return checkBatchMax(n, maxN, field)
}

// checkBatchMax is the ceiling alone, for the lists where empty is a question of
// its own: db_introspect without tables asks for the table list.
//
// An oversized batch is refused rather than trimmed: a trimmed list looks
// answered, and the questions that fell off are the ones nobody notices missing.
func checkBatchMax(n, maxN int, field string) error {
	if n > maxN {
		return fmt.Errorf("%s holds %d items, the limit is %d — split it into several calls", field, n, maxN)
	}
	return nil
}

// parallelMap runs f over every item at once, at most workers at a time, and
// returns the results in input order. No fail-fast: a batch answers per item.
//
// The worker limit keeps one batch from taking the whole instance — the upstream
// client has a single semaphore for every target, so twenty calls launched
// together would time out waiting for a slot rather than on their own merits.
func parallelMap[T, M any](a []T, workers int, f func(int, T) M) []M {
	out := make([]M, len(a))
	if workers < 1 {
		workers = 1
	}
	var g errgroup.Group
	g.SetLimit(workers)
	for i := range a {
		g.Go(func() (err error) {
			// The request middleware recovers the goroutine it serves on, and
			// this is not that goroutine.
			defer func() {
				if p := recover(); p != nil {
					err = fmt.Errorf("panic: %v", p)
				}
			}()
			// Each goroutine owns out[i] — no two indices overlap, no lock.
			out[i] = f(i, a[i])
			return nil
		})
	}
	// The only error f can produce is a recovered panic; that item stays at its
	// zero value, which the caller renders as an empty result.
	_ = g.Wait()
	return out
}

// dedup reports, for every item, the index of the first item with the same key,
// or -1 when this item is that first one. The model repeats itself inside one
// turn as readily as across turns — one trace held 42 verbatim repeats — and a
// repeat inside a batch is one the server can see without any memory at all.
func dedup[T any, K comparable](items []T, key func(T) K) []int {
	first := make(map[K]int, len(items))
	out := make([]int, len(items))
	for i, it := range items {
		k := key(it)
		if j, ok := first[k]; ok {
			out[i] = j
			continue
		}
		first[k] = i
		out[i] = -1
	}
	return out
}

// shareBudget hands out one byte budget over a batch, smallest first, and
// reports which items fit. Unspent bytes move to the next item, so a batch of
// small answers is never cut and one oversized answer starves only itself;
// dividing evenly up front would cut a two-line answer for bytes the wide one
// will not get anyway.
//
// An item that does not fit is dropped whole: half a JSON document is a parse
// error, not a smaller answer. The caller reports the drop with its real size.
func shareBudget(sizes []int, budget int) []bool {
	fits := make([]bool, len(sizes))
	order := make([]int, 0, len(sizes))
	for i, n := range sizes {
		if n >= 0 {
			order = append(order, i)
		}
	}
	// Sort by size, ties by index, so the same batch always cuts the same item.
	slices.SortFunc(order, func(a, b int) int {
		if sizes[a] != sizes[b] {
			return sizes[a] - sizes[b]
		}
		return a - b
	})
	left := max(budget, 0)
	for n, i := range order {
		// The smallest answer always fits: every item was already cut to its own
		// limit, and the size weighed here includes the scaffolding around it,
		// so an answer sitting exactly on its limit would otherwise be dropped
		// for the bytes of its own index and duration.
		if n > 0 && sizes[i] > left {
			continue
		}
		fits[i] = true
		left -= sizes[i]
	}
	return fits
}

var (
	batchItemsVec  *prometheus.HistogramVec
	batchItemsOnce sync.Once
)

// batchItems records how many items one tool call carried: whether clients
// actually batch is invisible from here otherwise, and a p50 of 1 would mean the
// nesting bought nothing.
//
// Its _count is the number of tools/call dispatches, observed as soon as the
// list is decoded, so a call refused for being too long is counted like any
// other. app_mcp_tool_calls_total is per item and counts upstream work instead.
func batchItems() *prometheus.HistogramVec {
	batchItemsOnce.Do(func() {
		batchItemsVec = prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: metricNamespace,
			Subsystem: metricSubsystem,
			Name:      "batch_items",
			Help:      "Items per MCP tool call; _count is the number of tools/call dispatches.",
			Buckets:   []float64{1, 2, 3, 5, 8, 12, 20},
		}, []string{"tool"})
		prometheus.MustRegister(batchItemsVec)
	})
	return batchItemsVec
}

// observeBatch records the size of one tool call's list.
func observeBatch(tool string, n int) {
	batchItems().WithLabelValues(tool).Observe(float64(n))
}
