// Copyright 2026 mvanhorn. Licensed under Apache-2.0. See LICENSE.

package cli

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"github.com/mvanhorn/printing-press-library/library/payments/prediction-goat/internal/store"
)

type comparePair struct {
	Topic    string        `json:"topic"`
	Match    float64       `json:"match,omitempty"`
	PM       *compareVenue `json:"polymarket,omitempty"`
	Kalshi   *compareVenue `json:"kalshi,omitempty"`
	DeltaPct *float64      `json:"deltaPct,omitempty"`
}

type compareVenue struct {
	ID             string  `json:"id"`
	Title          string  `json:"title"`
	YesProbability float64 `json:"yesProbability"`
	YesPercent     float64 `json:"yesPercent,omitempty"`
	Volume24h      float64 `json:"volume24h"`
	EndDate        string  `json:"endDate"`
	URL            string  `json:"url"`
	Untraded       bool    `json:"untraded,omitempty"`
}

type compareResult struct {
	Topic    string         `json:"topic"`
	Pairs    []comparePair  `json:"pairs"`
	Unpaired *compareUnpaired `json:"unpaired,omitempty"`
	Reason   string         `json:"reason,omitempty"`
}

// compareUnpaired surfaces the top hits per venue when pairing fails so
// the agent (or user) can pick an explicit pair via --pair instead of
// guessing whether the topic doesn't exist or just wasn't paired.
type compareUnpaired struct {
	Polymarket []compareVenue `json:"polymarket"`
	Kalshi     []compareVenue `json:"kalshi"`
}

type rawMarket struct {
	Venue          string
	ID             string
	Title          string
	YesProbability float64
	YesAsk         float64
	NoAsk          float64
	LastPrice      float64
	Volume24h      float64
	EndDate        string
	URL            string
	Untraded       bool
}

func newCompareCmd(flags *rootFlags) *cobra.Command {
	var limit int
	var dbPath string
	var pairOverride string
	cmd := &cobra.Command{
		Use:   "compare <topic>",
		Short: "Side-by-side Polymarket and Kalshi prices for a topic",
		Example: `  prediction-goat-pp-cli compare election --json
  prediction-goat-pp-cli compare 'arizona basketball' --limit 5
  prediction-goat-pp-cli compare 'Thunder Spurs' --pair will-the-oklahoma-city-thunder-win-the-2026-nba-finals=KXNBAWEST-26-OKC`,
		Annotations: map[string]string{"mcp:read-only": "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return cmd.Help()
			}
			if dryRunOK(flags) {
				return nil
			}
			if limit < 1 {
				return fmt.Errorf("compare: --limit must be greater than zero")
			}
			if dbPath == "" {
				dbPath = defaultDBPath("prediction-goat-pp-cli")
			}
			db, err := store.Open(dbPath)
			if err != nil {
				return fmt.Errorf("compare: %w", err)
			}
			defer db.Close()

			topic := strings.Join(args, " ")
			if pairOverride != "" {
				return runComparePair(cmd, flags, db, topic, pairOverride)
			}
			searchLimit := limit * 10
			if searchLimit < 50 {
				searchLimit = 50
			}
			pmMarkets, kalshiMarkets, err := loadCompareMarkets(cmd, db, topic, searchLimit)
			if err != nil {
				return fmt.Errorf("compare: %w", err)
			}
			pairs := pairCompareMarkets(topic, pmMarkets, kalshiMarkets, limit)
			// Drop unpaired-loose entries (no kalshi side) from the pairs
			// list when both sides exist independently. The unpaired
			// surface below captures the per-venue tops separately.
			truePairs := make([]comparePair, 0, len(pairs))
			for _, p := range pairs {
				if p.PM != nil && p.Kalshi != nil {
					truePairs = append(truePairs, p)
				}
			}
			result := compareResult{Topic: topic, Pairs: truePairs}
			if len(truePairs) == 0 {
				result.Unpaired = buildUnpaired(pmMarkets, kalshiMarkets, 5)
				if len(pmMarkets) == 0 && len(kalshiMarkets) == 0 {
					result.Reason = "no_topic_match"
				} else {
					result.Reason = "no_confident_pair"
				}
			}
			if flags.asJSON || !isTerminal(cmd.OutOrStdout()) {
				if err := printJSONFiltered(cmd.OutOrStdout(), result, flags); err != nil {
					return err
				}
			} else if err := printCompareTable(cmd.OutOrStdout(), truePairs); err != nil {
				return err
			}
			if len(truePairs) == 0 {
				return notFoundErr(fmt.Errorf("no Polymarket-Kalshi market pairs found for topic %q (reason: %s — see unpaired list for per-venue candidates, or pass --pair pm-slug=kalshi-ticker)", topic, result.Reason))
			}
			return nil
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 20, "Max pairs returned")
	cmd.Flags().StringVar(&dbPath, "db", "", "Database path (default: standard cache location)")
	cmd.Flags().StringVar(&pairOverride, "pair", "", "Explicit pm-slug=kalshi-ticker pair, skipping FTS-based pairing")
	return cmd
}

// buildUnpaired packages the top-N per venue as compareVenue rows for
// inclusion in the no-pair diagnostic surface.
func buildUnpaired(pm, kalshi []rawMarket, perVenue int) *compareUnpaired {
	out := &compareUnpaired{Polymarket: make([]compareVenue, 0), Kalshi: make([]compareVenue, 0)}
	for i, m := range pm {
		if i >= perVenue {
			break
		}
		out.Polymarket = append(out.Polymarket, compareVenueFromRaw(m))
	}
	for i, m := range kalshi {
		if i >= perVenue {
			break
		}
		out.Kalshi = append(out.Kalshi, compareVenueFromRaw(m))
	}
	return out
}

// runComparePair handles the --pair override path: skip FTS pairing,
// fetch each side by id, and emit a single pair. Reports
// explicit_pair_not_found when either side is missing from the store.
func runComparePair(cmd *cobra.Command, flags *rootFlags, db *store.Store, topic, override string) error {
	eq := strings.Index(override, "=")
	if eq <= 0 || eq == len(override)-1 {
		return usageErr(fmt.Errorf("compare: --pair must be pm-slug=kalshi-ticker (got %q)", override))
	}
	pmSlug := strings.TrimSpace(override[:eq])
	kalshiTicker := strings.TrimSpace(override[eq+1:])
	pmRow, pmOk := lookupRawMarket(cmd, db, "markets", pmSlug)
	kalshiRow, kalshiOk := lookupRawMarket(cmd, db, "kalshi_markets", kalshiTicker)
	if !pmOk || !kalshiOk {
		result := compareResult{Topic: topic, Reason: "explicit_pair_not_found"}
		if !pmOk || !kalshiOk {
			result.Unpaired = &compareUnpaired{Polymarket: []compareVenue{}, Kalshi: []compareVenue{}}
			if pmOk {
				result.Unpaired.Polymarket = append(result.Unpaired.Polymarket, compareVenueFromRaw(pmRow))
			}
			if kalshiOk {
				result.Unpaired.Kalshi = append(result.Unpaired.Kalshi, compareVenueFromRaw(kalshiRow))
			}
		}
		if flags.asJSON || !isTerminal(cmd.OutOrStdout()) {
			_ = printJSONFiltered(cmd.OutOrStdout(), result, flags)
		}
		return notFoundErr(fmt.Errorf("compare: --pair %s not found in local store (sync may be needed)", override))
	}
	pmVenue := compareVenueFromRaw(pmRow)
	kalshiVenue := compareVenueFromRaw(kalshiRow)
	delta := (pmRow.YesProbability - kalshiRow.YesProbability) * 100
	pair := comparePair{Topic: topic, PM: &pmVenue, Kalshi: &kalshiVenue, Match: 1.0, DeltaPct: &delta}
	result := compareResult{Topic: topic, Pairs: []comparePair{pair}}
	if flags.asJSON || !isTerminal(cmd.OutOrStdout()) {
		return printJSONFiltered(cmd.OutOrStdout(), result, flags)
	}
	return printCompareTable(cmd.OutOrStdout(), result.Pairs)
}

// lookupRawMarket fetches a single market row by resource type and id
// (slug for Polymarket, ticker for Kalshi). Used by the --pair override.
func lookupRawMarket(cmd *cobra.Command, db *store.Store, resourceType, id string) (rawMarket, bool) {
	row := db.DB().QueryRowContext(cmd.Context(), `SELECT data FROM resources WHERE resource_type=? AND id=? LIMIT 1`, resourceType, id)
	var data sql.NullString
	if err := row.Scan(&data); err != nil || !data.Valid {
		return rawMarket{}, false
	}
	return rawMarketFromJSON(resourceType, id, data.String)
}

func loadCompareMarkets(cmd *cobra.Command, db *store.Store, topic string, limit int) ([]rawMarket, []rawMarket, error) {
	// Fetch each venue independently so a venue with markedly higher
	// FTS5 token frequency for the topic can't crowd the other out via
	// a combined LIMIT. The single-query shape had a known failure mode
	// where the top `limit` rows could all come from one venue when its
	// indexed tokens dominated, causing pairCompareMarkets to find zero
	// pairable candidates even when relevant rows existed on both sides.
	// Mirrors the per-venue interleave used by topic/trending/liquid.
	pmMarkets, err := loadCompareMarketsByType(cmd, db, topic, "markets", limit)
	if err != nil {
		return nil, nil, err
	}
	kalshiMarkets, err := loadCompareMarketsByType(cmd, db, topic, "kalshi_markets", limit)
	if err != nil {
		return nil, nil, err
	}
	return pmMarkets, kalshiMarkets, nil
}

// loadCompareMarketsByType runs the FTS5 search restricted to one
// resource type so each venue contributes up to `limit` candidates
// before pairCompareMarkets pairs them by Jaccard similarity.
func loadCompareMarketsByType(cmd *cobra.Command, db *store.Store, topic, resourceType string, limit int) ([]rawMarket, error) {
	rows, err := db.DB().QueryContext(cmd.Context(), `SELECT r.resource_type, r.id, r.data FROM resources r
JOIN resources_fts f ON r.id = f.id AND r.resource_type = f.resource_type
WHERE resources_fts MATCH ?
AND r.resource_type = ?
ORDER BY rank LIMIT ?`, topicFTSQuery(topic), resourceType, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	markets := make([]rawMarket, 0)
	for rows.Next() {
		var typ, id, data sql.NullString
		if err := rows.Scan(&typ, &id, &data); err != nil {
			return nil, err
		}
		if !typ.Valid || !data.Valid {
			continue
		}
		market, ok := rawMarketFromJSON(typ.String, id.String, data.String)
		if !ok {
			continue
		}
		markets = append(markets, market)
	}
	return markets, rows.Err()
}

func pairCompareMarkets(topic string, pmMarkets, kalshiMarkets []rawMarket, limit int) []comparePair {
	pairs := make([]comparePair, 0)
	usedKalshi := make(map[int]bool)
	for _, pm := range pmMarkets {
		bestIdx := -1
		bestScore := 0.0
		for i, kalshi := range kalshiMarkets {
			if usedKalshi[i] {
				continue
			}
			if score := tokenJaccard(pm.Title, kalshi.Title); score > bestScore {
				bestIdx = i
				bestScore = score
			}
		}
		pmVenue := compareVenueFromRaw(pm)
		pair := comparePair{Topic: topic, PM: &pmVenue}
		if bestIdx >= 0 && bestScore >= 0.20 {
			usedKalshi[bestIdx] = true
			kalshiVenue := compareVenueFromRaw(kalshiMarkets[bestIdx])
			delta := (pm.YesProbability - kalshiMarkets[bestIdx].YesProbability) * 100
			pair.Kalshi = &kalshiVenue
			pair.Match = bestScore
			pair.DeltaPct = &delta
		}
		pairs = append(pairs, pair)
		if len(pairs) >= limit {
			return pairs
		}
	}
	for i, kalshi := range kalshiMarkets {
		if usedKalshi[i] {
			continue
		}
		kalshiVenue := compareVenueFromRaw(kalshi)
		pairs = append(pairs, comparePair{Topic: topic, Kalshi: &kalshiVenue})
		if len(pairs) >= limit {
			return pairs
		}
	}
	return pairs
}

func rawMarketFromJSON(resourceType, fallbackID, raw string) (rawMarket, bool) {
	var obj map[string]any
	if err := json.Unmarshal([]byte(raw), &obj); err != nil {
		return rawMarket{}, false
	}
	switch resourceType {
	case "markets":
		id := firstNonEmpty(jsonString(obj, "slug"), fallbackID)
		yes := jsonFloat(obj, "lastTradePrice")
		return rawMarket{Venue: "polymarket", ID: id, Title: firstNonEmpty(jsonString(obj, "question"), jsonString(obj, "title")), YesProbability: yes, LastPrice: yes, Volume24h: firstFloat(obj, "volume24h", "volume24hr", "volumeNum"), EndDate: jsonString(obj, "endDate"), URL: "https://polymarket.com/market/" + id}, true
	case "kalshi_markets":
		id := firstNonEmpty(jsonString(obj, "ticker"), fallbackID)
		eventTicker := jsonString(obj, "event_ticker")
		title := jsonString(obj, "title")
		// Multi-Variable Event markets carry comma-concatenated YES/NO
		// outcome legs in the `title` field; fall back to the parent
		// event ticker when that pattern is detected so downstream
		// comparison/matching uses a stable string.
		if looksLikeMultiLegTitle(title) && eventTicker != "" {
			title = eventTicker
		}
		yesAsk := jsonFloat(obj, "yes_ask_dollars")
		noAsk := jsonFloat(obj, "no_ask_dollars")
		lastPrice := jsonFloat(obj, "last_price_dollars")
		volume24h := jsonFloat(obj, "volume_24h_fp")
		untraded := isUntradedKalshi(yesAsk, noAsk, lastPrice, volume24h)
		return rawMarket{Venue: "kalshi", ID: id, Title: title, YesProbability: lastPrice, YesAsk: yesAsk, NoAsk: noAsk, LastPrice: lastPrice, Volume24h: volume24h, EndDate: jsonString(obj, "expiration_time"), URL: "https://kalshi.com/markets/" + eventTicker + "/" + id, Untraded: untraded}, true
	default:
		return rawMarket{}, false
	}
}

func compareVenueFromRaw(m rawMarket) compareVenue {
	return compareVenue{ID: m.ID, Title: m.Title, YesProbability: m.YesProbability, YesPercent: yesPercent(m.YesProbability), Volume24h: m.Volume24h, EndDate: m.EndDate, URL: m.URL, Untraded: m.Untraded}
}

func printCompareTable(w io.Writer, pairs []comparePair) error {
	tw := tabwriter.NewWriter(w, 2, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "Match\tPM Title\tKalshi Title\tPM%\tKalshi%\tDelta")
	for _, pair := range pairs {
		pmTitle, pmProb := "", ""
		kalshiTitle, kalshiProb := "", ""
		if pair.PM != nil {
			pmTitle = truncate(pair.PM.Title, 48)
			pmProb = formatProb(pair.PM.YesProbability)
		}
		if pair.Kalshi != nil {
			kalshiTitle = truncate(pair.Kalshi.Title, 48)
			kalshiProb = formatProb(pair.Kalshi.YesProbability)
		}
		delta := ""
		if pair.DeltaPct != nil {
			delta = fmt.Sprintf("%+.1f", *pair.DeltaPct)
		}
		fmt.Fprintf(tw, "%.2f\t%s\t%s\t%s\t%s\t%s\n", pair.Match, pmTitle, kalshiTitle, pmProb, kalshiProb, delta)
	}
	return tw.Flush()
}
