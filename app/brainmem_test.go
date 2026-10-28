package app

import (
	"strings"
	"testing"
	"time"
)

func TestParseOutcomes(t *testing.T) {
	batch := map[string]bool{"inbox/a": true, "inbox/b": true}
	tests := []struct {
		name   string
		answer string
		want   map[string]string
	}{
		{"reported", `{"slug":"inbox/a","outcome":"merged","into":"x"}` + "\n" + `{"slug":"inbox/b","outcome":"dropped"}`, map[string]string{"inbox/a": "merged", "inbox/b": "dropped"}},
		{"slug outside the batch is ignored", `{"slug":"entities/people","outcome":"dropped"}`, map[string]string{}},
		{"unknown outcome and prose are ignored", "Done!\n" + `{"slug":"inbox/a","outcome":"deleted"}`, map[string]string{}},
		{"last line for a slug wins", `{"slug":"inbox/a","outcome":"saved"}` + "\n" + `{"slug":"inbox/a","outcome":"dropped"}`, map[string]string{"inbox/a": "dropped"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseOutcomes(tt.answer, batch)
			if len(got) != len(tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
			for slug, want := range tt.want {
				if got[slug].Outcome != want {
					t.Errorf("%s = %q, want %q", slug, got[slug].Outcome, want)
				}
			}
		})
	}
}

func TestDigestNoticeCapsButtonsAndNamesEveryItem(t *testing.T) {
	var listed []outcomeRec
	for i := 0; i < maxDigestButtons+3; i++ {
		listed = append(listed, outcomeRec{brainOutcome: brainOutcome{Slug: "inbox/i" + string(rune('a'+i)), Outcome: outcomeSaved}, cursor: int64(i + 1)})
	}
	n := digestNotice(time.Unix(0, 0), listed, nil, 0)
	if len(n.Undo) != maxDigestButtons {
		t.Errorf("buttons = %d, want %d", len(n.Undo), maxDigestButtons)
	}
	for _, o := range listed {
		if !strings.Contains(n.Text, o.Slug) {
			t.Errorf("digest text lacks %s", o.Slug)
		}
	}
}
