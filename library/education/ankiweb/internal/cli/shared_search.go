// Copyright 2026 paul-bockewitz. Licensed under Apache-2.0. See LICENSE.

package cli

import (
	"encoding/json"
	"fmt"

	"github.com/mvanhorn/printing-press-library/library/education/ankiweb/internal/cliutil"
	"github.com/mvanhorn/printing-press-library/library/education/ankiweb/internal/store"
	"github.com/mvanhorn/printing-press-library/library/education/ankiweb/internal/svc"
	"github.com/spf13/cobra"
)

func newSharedSearchCmd(flags *rootFlags) *cobra.Command {
	var flagSearch string
	var hasAudio bool
	var hasImages bool

	cmd := &cobra.Command{
		Use:         "search [term]",
		Short:       "Search the shared-deck catalog by keyword",
		Example:     "  ankiweb-pp-cli shared search spanish --has-audio\n  ankiweb-pp-cli shared search --search japanese --json",
		Annotations: map[string]string{"pp:endpoint": "shared.search", "pp:method": "GET", "pp:path": "/svc/shared/list-decks", "mcp:read-only": "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			term := flagSearch
			if term == "" && len(args) > 0 {
				term = args[0]
			}
			if term == "" {
				return cmd.Help()
			}
			if dryRunOK(flags) {
				fmt.Fprintf(cmd.OutOrStdout(), "GET %s/svc/shared/list-decks?search=%s\n", "https://ankiweb.net", term)
				return nil
			}
			if cliutil.IsVerifyEnv() {
				return printJSONFiltered(cmd.OutOrStdout(), []svc.SharedDeck{}, flags)
			}

			var decks []svc.SharedDeck
			if flags.dataSource == "local" {
				// Offline: FTS the rows synced into the local `shared` table.
				decks, err := searchSharedLocal(cmd, flags, term, hasAudio, hasImages)
				if err != nil {
					return err
				}
				return printJSONFiltered(cmd.OutOrStdout(), decks, flags)
			}

			c, _, err := flags.newSvcClient()
			if err != nil {
				return err
			}
			decks, err = listDecks(cmd.Context(), c, term)
			if err != nil {
				return classifyAPIError(err, flags)
			}

			filtered := decks[:0]
			for _, d := range decks {
				if hasAudio && d.Audio <= 0 {
					continue
				}
				if hasImages && d.Images <= 0 {
					continue
				}
				filtered = append(filtered, d)
			}
			return printJSONFiltered(cmd.OutOrStdout(), filtered, flags)
		},
	}
	cmd.Flags().StringVar(&flagSearch, "search", "", "Search term (e.g. spanish, anatomy, MCAT). Also accepted as a positional arg.")
	cmd.Flags().BoolVar(&hasAudio, "has-audio", false, "Keep only decks that include audio")
	cmd.Flags().BoolVar(&hasImages, "has-images", false, "Keep only decks that include images")

	return cmd
}

// searchSharedLocal full-text searches the locally synced `shared` table,
// applying the same audio/image filters as the live path. Used under
// --data-source local for offline operation.
func searchSharedLocal(cmd *cobra.Command, flags *rootFlags, term string, hasAudio, hasImages bool) ([]svc.SharedDeck, error) {
	dbPath := defaultDBPath("ankiweb-pp-cli")
	db, err := store.OpenWithContext(cmd.Context(), dbPath)
	if err != nil {
		return nil, fmt.Errorf("opening local store: %w", err)
	}
	defer db.Close()

	hintIfUnsynced(cmd, db, "shared")
	rows, err := db.SearchShared(term, 1000)
	if err != nil {
		return nil, fmt.Errorf("searching local store: %w", err)
	}
	out := make([]svc.SharedDeck, 0, len(rows))
	for _, raw := range rows {
		var d svc.SharedDeck
		if json.Unmarshal(raw, &d) != nil {
			continue
		}
		if hasAudio && d.Audio <= 0 {
			continue
		}
		if hasImages && d.Images <= 0 {
			continue
		}
		out = append(out, d)
	}
	return out, nil
}
