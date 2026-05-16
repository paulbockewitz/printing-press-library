// Copyright 2026 gregce. Licensed under Apache-2.0. See LICENSE.

// PATCH(library): hand-added composition — Tella Cut-panel "Remove buffers" has
// no public-API equivalent (verified via 404 smoke test against api.tella.com
// on 2026-05-16: remove-buffers, trim-buffers, cut-buffers all return 404 while
// baseline remove-fillers returns 200). This file composes the documented
// primitives `GET /v1/videos/{id}/clips/{clipId}`, `GET .../silences`, and
// `POST .../cut` to deliver the head-and-tail silence trim that "Remove
// buffers" implies in the user's task description. Cataloged in
// .printing-press-patches.json under id `add-remove-buffers-composition`.

package cli

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"
)

// headBufferToleranceMs is the maximum startTimeMs a silence can have and
// still be classified as a leading buffer. 50ms accommodates float
// precision in the silences endpoint without picking up early-but-after-
// speech pauses.
const headBufferToleranceMs = 50

// tailBufferToleranceMs is how close a silence's end must be to the clip's
// total duration to be classified as a trailing buffer. 100ms is wider than
// the head tolerance to absorb float precision in the clip's durationSeconds
// field (which is reported as a float in seconds).
const tailBufferToleranceMs = 100

func newVideosClipsRemoveBuffersCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "remove-buffers <id> <clipId>",
		Short:   "Trim leading and trailing silence from a clip by posting cuts for the head and tail silence ranges. Composes /silences + /cut. Returns a structured summary.",
		Example: "  tella-pp-cli videos clips remove-buffers vid_abc cl_xyz",
		// No pp:endpoint annotation: this is a multi-call composition, not a
		// single endpoint. cobratree.RegisterAll() will still surface it as a
		// shell-out MCP tool (classify.go only skips endpoint-annotated cmds).
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) < 2 {
				_ = cmd.Help()
				return usageErr(fmt.Errorf("usage: %s <id> <clipId>", cmd.CommandPath()))
			}
			videoID, clipID := args[0], args[1]
			c, err := flags.newClient()
			if err != nil {
				return err
			}
			// The read path (clip + silences) needs real data even when
			// --dry-run is set, otherwise we can't compute the plan. Disable
			// the client's DryRun gate for this command and instead check
			// flags.dryRun ourselves before the POST step below.
			c.DryRun = false

			clipDurationMs, err := fetchClipDurationMs(c, videoID, clipID)
			if err != nil {
				return classifyAPIError(err, flags)
			}

			silData, err := c.Get(fmt.Sprintf("/v1/videos/%s/clips/%s/silences", videoID, clipID), nil)
			if err != nil {
				return classifyAPIError(err, flags)
			}
			ranges := parseSilencesMs(silData)
			head, tail := pickBufferRanges(ranges, clipDurationMs)

			type appliedCut struct {
				FromMs int    `json:"fromMs"`
				ToMs   int    `json:"toMs"`
				Reason string `json:"reason"`
				Status int    `json:"status,omitempty"`
				Error  string `json:"error,omitempty"`
			}
			result := map[string]any{
				"video_id":         videoID,
				"clip_id":          clipID,
				"clip_duration_ms": clipDurationMs,
			}

			plan := []appliedCut{}
			if head != nil {
				plan = append(plan, appliedCut{FromMs: head.Start, ToMs: head.End, Reason: "head-buffer"})
			}
			if tail != nil {
				plan = append(plan, appliedCut{FromMs: tail.Start, ToMs: tail.End, Reason: "tail-buffer"})
			}
			result["planned"] = plan

			if flags.dryRun {
				result["dry_run"] = true
				result["applied"] = false
				return printJSONFiltered(cmd.OutOrStdout(), result, flags)
			}

			applied := []appliedCut{}
			succeeded, failed := 0, 0
			for _, p := range plan {
				_, status, postErr := c.Post(
					fmt.Sprintf("/v1/videos/%s/clips/%s/cut", videoID, clipID),
					map[string]any{"fromMs": p.FromMs, "toMs": p.ToMs},
				)
				if postErr != nil {
					failed++
					p.Error = postErr.Error()
					applied = append(applied, p)
					continue
				}
				succeeded++
				p.Status = status
				applied = append(applied, p)
			}
			result["applied"] = true
			result["applied_ops"] = succeeded
			result["failed_ops"] = failed
			result["cuts"] = applied
			return printJSONFiltered(cmd.OutOrStdout(), result, flags)
		},
	}
	return cmd
}

// fetchClipDurationMs reads clip metadata and returns the clip's total
// duration in milliseconds. The public API reports `durationSeconds` as a
// float; multiplying by 1000 and rounding is the cleanest conversion.
// Returns 0 with an error if the response can't be parsed; callers must
// classify the error before returning to the user.
func fetchClipDurationMs(c clipDurationGetter, videoID, clipID string) (int, error) {
	data, err := c.Get(fmt.Sprintf("/v1/videos/%s/clips/%s", videoID, clipID), nil)
	if err != nil {
		return 0, err
	}
	// API wraps the clip in a top-level `clip` key.
	var env struct {
		Clip struct {
			DurationSeconds float64 `json:"durationSeconds"`
		} `json:"clip"`
	}
	if uerr := json.Unmarshal(data, &env); uerr != nil {
		return 0, fmt.Errorf("parsing clip response: %w", uerr)
	}
	if env.Clip.DurationSeconds <= 0 {
		return 0, fmt.Errorf("clip %s/%s has no positive durationSeconds in the API response — cannot compute tail buffer", videoID, clipID)
	}
	return int(env.Clip.DurationSeconds*1000 + 0.5), nil
}

// clipDurationGetter is the minimum surface fetchClipDurationMs needs. The
// real client satisfies it; tests substitute a stub.
type clipDurationGetter interface {
	Get(path string, params map[string]string) (json.RawMessage, error)
}

// parseSilencesMs reads the public `GET /silences` response, which today
// returns objects shaped `{startTimeMs, durationMs}` (verified against the
// live API on 2026-05-16). The existing extractSilenceRanges helper in
// clips_edit_pass.go expects `{start, end}` / `{startMs, endMs}` shapes that
// the API no longer emits, so a dedicated parser keeps this composition
// honest without touching that helper.
//
// Floats are rounded to the nearest millisecond. Ranges with non-positive
// duration are skipped. The returned slice preserves API order.
func parseSilencesMs(data json.RawMessage) []silenceRange {
	var env struct {
		Silences []map[string]any `json:"silences"`
	}
	if err := json.Unmarshal(data, &env); err != nil || env.Silences == nil {
		// Some responses come back as a bare array rather than {silences:[...]}
		// — handle both.
		var bare []map[string]any
		if berr := json.Unmarshal(data, &bare); berr == nil {
			env.Silences = bare
		}
	}
	out := make([]silenceRange, 0, len(env.Silences))
	for _, item := range env.Silences {
		start := floatField(item, "startTimeMs", "startMs", "start")
		dur := floatField(item, "durationMs", "duration")
		if dur <= 0 {
			continue
		}
		end := start + dur
		out = append(out, silenceRange{
			Start: int(start + 0.5),
			End:   int(end + 0.5),
		})
	}
	return out
}

// floatField is a millisecond-aware sibling of intField that preserves
// fractional precision for the start+duration arithmetic before rounding.
func floatField(m map[string]any, keys ...string) float64 {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			switch x := v.(type) {
			case float64:
				return x
			case int:
				return float64(x)
			}
		}
	}
	return 0
}

// pickBufferRanges classifies silence ranges into head and tail buffers.
// Returns (head, tail), either of which may be nil if no qualifying range
// exists. The tolerance constants above are intentionally tight: a silence
// "in the middle of the clip" should never be picked.
func pickBufferRanges(ranges []silenceRange, clipDurationMs int) (head *silenceRange, tail *silenceRange) {
	for i := range ranges {
		r := ranges[i]
		if r.End <= r.Start {
			continue
		}
		if r.Start <= headBufferToleranceMs {
			// First qualifying head wins; later short pauses don't replace it.
			if head == nil {
				h := r
				head = &h
			}
		}
		if clipDurationMs > 0 && (clipDurationMs-r.End) <= tailBufferToleranceMs {
			// Last qualifying tail wins so we always grab the farthest-right
			// silence touching the clip's end.
			t := r
			tail = &t
		}
	}
	return head, tail
}
