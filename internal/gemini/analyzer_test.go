package gemini

import (
	"encoding/json"
	"testing"
)

func TestVerdictMatches(t *testing.T) {
	const threshold = 65

	tests := []struct {
		name    string
		verdict Verdict
		want    bool
	}{
		{"remote above threshold", Verdict{Score: 80, Remote: RemoteYes}, true},
		{"remote at threshold", Verdict{Score: 65, Remote: RemoteYes}, true},
		{"remote below threshold", Verdict{Score: 64, Remote: RemoteYes}, false},
		{
			// The remote-only rule is the candidate's hard constraint, so a
			// high score must not be able to override it.
			name:    "onsite with perfect score",
			verdict: Verdict{Score: 100, Remote: RemoteOnsite},
			want:    false,
		},
		{"hybrid with high score", Verdict{Score: 95, Remote: RemoteHybrid}, false},
		{"unknown format above threshold", Verdict{Score: 70, Remote: RemoteUnknown}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.verdict.Matches(threshold); got != tt.want {
				t.Fatalf("Matches() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestNormalize(t *testing.T) {
	tests := []struct {
		name       string
		in         Verdict
		wantScore  int
		wantRemote string
	}{
		{"clamps above 100", Verdict{Score: 140, Remote: "remote"}, 100, RemoteYes},
		{"clamps below 0", Verdict{Score: -5, Remote: "remote"}, 0, RemoteYes},
		{"russian remote", Verdict{Score: 70, Remote: "Удалённо"}, 70, RemoteYes},
		{"russian hybrid", Verdict{Score: 70, Remote: "гибрид"}, 70, RemoteHybrid},
		{"russian office", Verdict{Score: 70, Remote: "офис"}, 70, RemoteOnsite},
		{"unrecognised value", Verdict{Score: 70, Remote: "maybe"}, 70, RemoteUnknown},
		{"empty value", Verdict{Score: 70}, 70, RemoteUnknown},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Normalize(tt.in)
			if got.Score != tt.wantScore {
				t.Errorf("Score = %d, want %d", got.Score, tt.wantScore)
			}
			if got.Remote != tt.wantRemote {
				t.Errorf("Remote = %q, want %q", got.Remote, tt.wantRemote)
			}
		})
	}
}

func TestExtractJSONObject(t *testing.T) {
	const want = `{"score": 82, "remote": "remote", "summary": "подходит"}`

	tests := []struct {
		name    string
		in      string
		wantErr bool
	}{
		{"bare object", want, false},
		{"markdown fenced", "```json\n" + want + "\n```", false},
		{"prose around it", "Вот результат:\n" + want + "\nНадеюсь, помог.", false},
		{"no object", "MATCH: yes", true},
		{"unbalanced", `{"score": 82`, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ExtractJSONObject(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != want {
				t.Fatalf("got %q, want %q", got, want)
			}
		})
	}
}

// A brace inside a string value must not end the object early — the model
// quotes post text into "summary" often enough for this to matter.
func TestExtractJSONObjectWithBraceInString(t *testing.T) {
	in := `{"score": 10, "remote": "onsite", "summary": "пост содержит {скобку}"}`
	got, err := ExtractJSONObject("prefix " + in + " suffix")
	if err != nil {
		t.Fatal(err)
	}
	if got != in {
		t.Fatalf("got %q, want %q", got, in)
	}

	var v Verdict
	if err := json.Unmarshal([]byte(got), &v); err != nil {
		t.Fatal(err)
	}
	if v.Summary != "пост содержит {скобку}" {
		t.Fatalf("summary = %q", v.Summary)
	}
}
