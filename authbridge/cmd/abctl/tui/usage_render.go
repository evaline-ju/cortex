package tui

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/rossoctl/cortex/authbridge/authlib/usage"
)

// Bar geometry. Four columns wide with a one-column gap, so ten bars occupy 49
// columns and fit an 80-column terminal alongside the axis gutter.
//
// The gap earns its column twice over: adjacent bars blur into one mass at the
// tall end, and in a stacked view a boundary BETWEEN bars would otherwise be
// indistinguishable from a boundary WITHIN one. Four is also the practical floor
// for stacked segments — narrower and the block glyphs stop reading as distinct
// textures, which is what carries the encoding when color is unavailable.
const (
	barWidth = 4
	// barGap is 2, not 1, so a value label always has a separating column. Labels
	// are up to 5 wide ("48.2k", "1.2ms"); at a stride of 5 two adjacent labels
	// touched and read as one nonsense number ("1.2ms0"). Widening the gap keeps
	// every label — alternating them instead would have dropped the newest
	// bucket's value on a narrow terminal and hidden the "0" that distinguishes an
	// idle minute from a small one. Ten bars at stride 6 plus the gutter is 66
	// columns, still inside 80.
	barGap    = 2
	barStride = barWidth + barGap
	plotRows  = 10 // vertical resolution before partial blocks
	axisLabel = 6  // "  50k " gutter
)

// eighths are the vertical partial blocks, 1/8 through 8/8. They give the
// ungrouped view 8x the vertical resolution of whole cells for free: a bar's top
// cell renders its fractional row rather than rounding away up to a full row of
// value.
var eighths = [...]rune{'▁', '▂', '▃', '▄', '▅', '▆', '▇', '█'}

// usageMetric selects which series the bar chart plots.
type usageMetric int

const (
	metricTokens usageMetric = iota
	metricRequests
	metricErrors
	metricLatency

	// usageMetricCount bounds the [t] cycle. Kept adjacent to the iota block so
	// adding a metric means editing one line here.
	usageMetricCount = iota
)

func (m usageMetric) String() string {
	switch m {
	case metricRequests:
		return "requests"
	case metricErrors:
		return "errors"
	case metricLatency:
		return "latency"
	default:
		return "tokens"
	}
}

// isLatency reports whether this metric needs the whiskers renderer rather than
// bars. Latency is a distribution, not a magnitude from zero, so it gets a
// different form — see the comment on the whisker glyphs.
func (m usageMetric) isLatency() bool { return m == metricLatency }

// valueOf extracts this metric from a per-label Counts. Bucket embeds Counts, so
// value below delegates here — one place decides what each metric means, and a
// stacked segment can never disagree with the bar it sits inside.
func (m usageMetric) valueOf(c usage.Counts) int64 {
	switch m {
	case metricRequests:
		return c.Requests
	case metricErrors:
		return c.Errors
	default:
		return c.Tokens
	}
}

// value extracts this metric from a bucket.
func (m usageMetric) value(b usage.Bucket) int64 {
	return m.valueOf(b.Counts)
}

// renderBars draws the ungrouped bar chart: a y-axis with humanized labels, one
// bar per bucket using partial blocks for sub-row precision, a time axis, and a
// value row.
//
// Pure by design — buckets in, lines out, no terminal and no model state — so
// the exact glyph output is table-testable. Terminal rendering bugs are
// otherwise only visible by eye, and the two cases that matter most (an idle
// bucket versus a very small one; a fractional top cell) are precisely the ones
// eyes skip over.
func renderBars(buckets []usage.Bucket, m usageMetric, width int) []string {
	if len(buckets) == 0 {
		return []string{"  (no data)"}
	}
	// Fit as many bars as the terminal allows, keeping the newest: an operator
	// watching live traffic cares about now, not about the start of the window.
	maxBars := (width - axisLabel) / barStride
	if maxBars < 1 {
		maxBars = 1
	}
	if len(buckets) > maxBars {
		buckets = buckets[len(buckets)-maxBars:]
	}

	var peak int64
	for _, b := range buckets {
		if v := m.value(b); v > peak {
			peak = v
		}
	}

	out := make([]string, 0, plotRows+3)

	// Plot rows, top down. Each row is a threshold; a bar fills the row when its
	// value reaches the row's ceiling, and renders a partial block when it lands
	// inside the row.
	for row := plotRows; row >= 1; row-- {
		var sb strings.Builder
		// Label every other row, matching the axis tick density below.
		if row%2 == 0 && peak > 0 {
			sb.WriteString(fmt.Sprintf("%5s ", humanizeCount(peak*int64(row)/int64(plotRows))))
		} else {
			sb.WriteString(strings.Repeat(" ", axisLabel))
		}
		sb.WriteString(barCellsForRow(buckets, m, peak, row))
		out = append(out, strings.TrimRight(sb.String(), " "))
	}

	out = append(out, renderAxis(len(buckets)))
	out = append(out, renderTimeLabels(buckets))
	out = append(out, renderValues(buckets, m))
	return out
}

// barCellsForRow renders one horizontal slice of every bar.
func barCellsForRow(buckets []usage.Bucket, m usageMetric, peak int64, row int) string {
	var sb strings.Builder
	for _, b := range buckets {
		v := m.value(b)
		sb.WriteString(barCell(v, peak, row))
		sb.WriteString(strings.Repeat(" ", barGap))
	}
	return sb.String()
}

// barCell renders one bar's glyphs for one row: full blocks below the value, a
// partial block at the fractional boundary, spaces above.
func barCell(v, peak int64, row int) string {
	if v <= 0 || peak <= 0 {
		return strings.Repeat(" ", barWidth)
	}
	// Height in eighths of a row, so a bar shorter than one row still shows.
	totalEighths := v * int64(plotRows) * 8 / peak
	// Floor at one eighth: integer division truncates a small-but-nonzero value
	// to nothing (50 against a 50k peak is 0.08 eighths), which would render
	// real traffic as an empty column — indistinguishable from the idle bucket
	// the value row deliberately marks "0". A visible sliver is the honest
	// answer; invisibility is not.
	if totalEighths == 0 {
		totalEighths = 1
	}
	rowFloor := int64(row-1) * 8
	inThisRow := totalEighths - rowFloor
	switch {
	case inThisRow >= 8:
		return strings.Repeat("█", barWidth)
	case inThisRow <= 0:
		return strings.Repeat(" ", barWidth)
	default:
		return strings.Repeat(string(eighths[inThisRow-1]), barWidth)
	}
}

// renderAxis draws the baseline. The corner glyph sits in the LAST gutter column
// so the dashes after it start at axisLabel — the column the bars start at.
// Writing axisLabel-2 spaces then "0 ┼" placed the corner AT axisLabel and
// shifted every tick one column right of the bar it underlines.
func renderAxis(n int) string {
	var sb strings.Builder
	// Right-align "0" in the gutter, leaving the final cell for the corner.
	sb.WriteString(strings.Repeat(" ", axisLabel-3))
	sb.WriteString("0 ┼")
	for i := 0; i < n; i++ {
		sb.WriteString(strings.Repeat("─", barWidth))
		if i < n-1 {
			// One tick plus dashes for the rest of the gap, so the tick stays on
			// the bar boundary and the baseline remains continuous whatever barGap
			// is set to.
			sb.WriteString("┴")
			sb.WriteString(strings.Repeat("─", barGap-1))
		}
	}
	return sb.String()
}

// renderTimeLabels labels every other bucket. A full HH:MM does not fit under a
// 4-column bar, so the first label carries the hour and the rest are minutes
// only — enough to place a spike once the reader has the hour.
func renderTimeLabels(buckets []usage.Bucket) string {
	// Painted into a column-indexed buffer rather than appended: a label is
	// wider than the 4-column bar it belongs to, so consecutive labels would
	// otherwise push each other rightward and drift out of alignment with the
	// bars they name. Placing by absolute column keeps every label under its own
	// bar no matter how wide the previous one was.
	row := make([]byte, axisLabel+len(buckets)*barStride+8)
	for i := range row {
		row[i] = ' '
	}
	for i, b := range buckets {
		if i%2 != 0 {
			continue
		}
		label := b.At.Format(":04")
		if i == 0 {
			label = b.At.Format("15:04")
		}
		at := axisLabel + i*barStride
		if at+len(label) <= len(row) {
			copy(row[at:], label)
		}
	}
	return strings.TrimRight(string(row), " ")
}

// renderValues prints each bucket's value under its bar. An idle bucket prints
// "0" rather than blank: a chart with a missing bar cannot otherwise distinguish
// no traffic from traffic too small to plot, and that is exactly the gap an
// operator is hunting when usage looks wrong.
func renderValues(buckets []usage.Bucket, m usageMetric) string {
	// Column-painted for the same reason as renderTimeLabels: a 5-character
	// value under a 4-column bar would otherwise shove its neighbours right.
	row := make([]byte, axisLabel+len(buckets)*barStride+8)
	for i := range row {
		row[i] = ' '
	}
	for i, b := range buckets {
		v := m.value(b)
		label := "0" // an idle bucket is stated, never blank
		if v != 0 {
			label = humanizeCount(v)
		}
		at := axisLabel - 1 + i*barStride
		if at+len(label) <= len(row) {
			copy(row[at:], label)
		}
	}
	return strings.TrimRight(string(row), " ")
}

// maxCountLabelLen is the width humanizeCount promises. renderBars lays out the
// axis gutter and value row against it, so exceeding it pushes lines past the
// terminal width and wraps the chart.
const maxCountLabelLen = 5

// humanizeCount renders a count in at most maxCountLabelLen characters.
//
// Every magnitude has to be covered, not just the plausible ones: the previous
// version fell through to "%.1fM" above a million, so a billion tokens rendered
// as "1000.0M" — seven characters — and a large enough total silently broke the
// layout. Token counts over a 6h window are exactly the figure that grows without
// anyone revisiting this function.
func humanizeCount(v int64) string {
	switch {
	case v < 0:
		return "0"
	case v < 1000:
		return fmt.Sprintf("%d", v) // 0..999
	case v < 10_000:
		return fmt.Sprintf("%.1fk", float64(v)/1000) // 1.0k..9.9k
	case v < 1_000_000:
		return fmt.Sprintf("%dk", v/1000) // 10k..999k
	case v < 10_000_000:
		return fmt.Sprintf("%.1fM", float64(v)/1e6) // 1.0M..9.9M
	case v < 1_000_000_000:
		return fmt.Sprintf("%dM", v/1e6) // 10M..999M
	case v < 10_000_000_000:
		return fmt.Sprintf("%.1fG", float64(v)/1e9) // 1.0G..9.9G
	case v < 1_000_000_000_000:
		return fmt.Sprintf("%dG", v/1e9) // 10G..999G
	case v < 10_000_000_000_000:
		return fmt.Sprintf("%.1fT", float64(v)/1e12) // 1.0T..9.9T
	case v < 1_000_000_000_000_000:
		return fmt.Sprintf("%dT", v/1e12) // 10T..999T
	default:
		// Past 999T an int64 has about four decades left; clamp rather than
		// widen, since a chart is unreadable long before this.
		return ">999T"
	}
}

// renderUsageSummary is the footer line: totals plus latency and cost.
func renderUsageSummary(snap *usage.Snapshot) string {
	var parts []string
	parts = append(parts, fmt.Sprintf("REQUESTS %d", snap.Totals.Requests))

	errPct := ""
	if snap.Totals.Requests > 0 {
		errPct = fmt.Sprintf(" (%.1f%%)", 100*float64(snap.Totals.Errors)/float64(snap.Totals.Requests))
	}
	parts = append(parts, fmt.Sprintf("ERRORS %d%s", snap.Totals.Errors, errPct))
	parts = append(parts, fmt.Sprintf("TOKENS %s", humanizeCount(snap.Totals.Tokens)))

	// Latency across the window, weighted by request count so a quiet minute
	// does not count as much as a busy one (the same mean-of-means trap the
	// server avoids when folding).
	var latSum float64
	var latN int64
	for _, b := range snap.Buckets {
		// Weight by LatSamples (requests that carried a duration), not Requests:
		// an unmeasured response is traffic but not a latency sample, and using
		// Requests understates the mean in proportion to how many there were.
		if b.LatMeanMs > 0 && b.LatSamples > 0 {
			latSum += b.LatMeanMs * float64(b.LatSamples)
			latN += b.LatSamples
		}
	}
	if latN > 0 {
		parts = append(parts, fmt.Sprintf("LATENCY %.2fs", latSum/float64(latN)/1000))
	}

	parts = append(parts, renderCostSummary(snap))
	return "  " + strings.Join(parts, "    ")
}

// renderCostSummary is the COST cell, in one of three states.
//
// Nothing priced: say so rather than render $0.00, which reads as "this traffic
// was free" — a zero cost and an unknown cost are different answers.
//
// Partially priced: show the figure AND the coverage, because the total only
// covers the requests that carried a cost. Cost comes from litellm-budget-track,
// which may not be in the pipeline for all traffic and cannot price every
// endpoint, so a window mixing priced and unpriced requests is the normal case
// rather than an edge one. Presenting its subtotal as if it were the whole spend
// is the failure this coverage count exists to prevent.
func renderCostSummary(snap *usage.Snapshot) string {
	if !snap.Priced {
		// Not "$0.0000". A zero cost and an unknown cost are different answers, and
		// only one of them means the traffic was free.
		return "COST unavailable"
	}
	// Integer cent rounding, not %.2f on float64(micros)/1e6: 1_005_000 micros
	// is exactly $1.005 but the float is 1.0049999… so %.2f prints $1.00.
	// A positive-but-sub-cent total falls back to "<$0.01" so small does not
	// read as free — same floor rule formatUSDCell applies at $0.0001.
	micros := snap.Totals.CostMicros
	var cell string
	switch {
	case micros < 0:
		// Go's / and % truncate toward zero, so a negative would render
		// "$0.-1" through the integer-cents branch below.
		cell = "COST unavailable"
	case micros > 0 && micros < 5_000:
		cell = "COST <$0.01"
	default:
		cents := micros / 10_000
		if micros%10_000 >= 5_000 {
			cents++
		}
		cell = fmt.Sprintf("COST $%d.%02d", cents/100, cents%100)
	}
	// Compared against PRICEABLE requests, not all of them. Requests counts every
	// proxied response — MCP tool calls, health checks, anything else the sidecar
	// handled — while only inference can ever be priced, so the old ratio left a
	// correctly configured deployment reading "1/10 priced" forever with an empty gap
	// list. A permanent warning with nothing to act on trains an operator to ignore
	// the one signal that matters.
	// Provenance qualifies the figure: $12.40 from a gateway's own numbers and $12.40
	// modelled from a shipped vendor-list table are not equally trustworthy, and the
	// column showed them identically. Only shown when it tells the reader something —
	// a single uniform provenance that is authoritative needs no annotation.
	cell += provenanceNote(snap.PricedBy)

	priceable := snap.Totals.PriceableRequests
	if priceable == 0 || snap.Totals.PricedRequests >= priceable {
		return cell
	}
	// Partial coverage: the total covers only the priced subset, so say so, and
	// name what is missing. "Cost is incomplete" is not actionable; the endpoint and
	// model are exactly what an operator needs to write a pricing entry for.
	cell += fmt.Sprintf(" (%d/%d priced", snap.Totals.PricedRequests, priceable)
	if gaps := topUnpriced(snap.UnpricedBy, 3); gaps != "" {
		cell += "; unpriced: " + gaps
	}
	return cell + ")"
}

// topUnpriced names the biggest coverage gaps, largest first, capped so one
// pathological deployment cannot push the rest of the line off screen.
//
// Ordered by count rather than alphabetically because an operator fixing coverage
// wants the entry that buys the most first. Ties break on the key so the line does
// not reshuffle between refreshes for no reason.
func topUnpriced(by map[string]int64, max int) string {
	if len(by) == 0 {
		return ""
	}
	keys := make([]string, 0, len(by))
	for k := range by {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if by[keys[i]] != by[keys[j]] {
			return by[keys[i]] > by[keys[j]]
		}
		return keys[i] < keys[j]
	})
	shown := keys
	if len(shown) > max {
		shown = shown[:max]
	}
	parts := make([]string, 0, len(shown))
	for _, k := range shown {
		parts = append(parts, fmt.Sprintf("%s x%d", sanitizeLabel(k), by[k]))
	}
	out := strings.Join(parts, ", ")
	if rest := len(keys) - len(shown); rest > 0 {
		out += fmt.Sprintf(", +%d more", rest)
	}
	return out
}

// usageWindows are the spans the [w] key cycles.
var usageWindows = []struct {
	window     time.Duration
	resolution time.Duration
}{
	{10 * time.Minute, time.Minute},
	{time.Hour, 5 * time.Minute},
	{6 * time.Hour, 30 * time.Minute},
}

// provenanceNote renders where a cost total came from, or "" when saying so would
// add nothing.
//
// Silent for a wholly authoritative total, because "the gateway told us" is the
// baseline a reader already assumes. Loud for anything modelled, and loudest when
// mixed — a partly-modelled total is the case where a reader most needs to know
// which part to trust.
func provenanceNote(by map[string]int64) string {
	if len(by) == 0 {
		return ""
	}
	if len(by) == 1 {
		for k := range by {
			if k == "authoritative" {
				return ""
			}
			return " [" + k + "]"
		}
	}
	// Mixed: list every level, largest first, so the dominant source reads first.
	keys := make([]string, 0, len(by))
	for k := range by {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if by[keys[i]] != by[keys[j]] {
			return by[keys[i]] > by[keys[j]]
		}
		return keys[i] < keys[j]
	})
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s %d", k, by[k]))
	}
	return " [" + strings.Join(parts, ", ") + "]"
}

// sanitizeLabel makes a wire-derived label safe to write to a terminal.
//
// These keys are "<endpoint> <model>", and the model half comes from the `model` field
// of the request body — chosen by the workload, and the inference parser records it
// verbatim. Writing it straight to a TTY lets an escape sequence reposition the cursor,
// recolour the pane, or erase the very coverage gap it is reporting; a newline alone
// breaks the table apart. CWE-150.
//
// Control characters and DEL become U+FFFD rather than being dropped, so tampering is
// visible instead of silently producing a plausible-looking label.
func sanitizeLabel(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == 0x7f, r < 0x20:
			b.WriteRune('\uFFFD')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
