package main

import (
	"strings"
	"testing"
)

func TestClassify(t *testing.T) {
	cases := []struct {
		msg  string
		want Bump
	}{
		{"feat: add webhook", Minor},
		{"feat(api): add webhook", Minor},
		{"FEAT: shouting still counts", Minor},
		{"fix: handle 409", Patch},
		{"fix(relay): fence results", Patch},
		{"perf: faster claims", Patch},
		{"refactor: split service", Patch},
		{"docs: explain releases", None},
		{"chore(deps): bump pgx", None},
		{"ci: cache modules", None},
		{"test: cover scheduler", None},
		{"style: gofmt", None},
		{"build: tweak Dockerfile", None},
		{"feat!: drop legacy routes", Major},
		{"fix(api)!: change error body", Major},
		{"docs!: even docs can break", Major},
		{"feat: new thing\n\nBREAKING CHANGE: the old thing is gone", Major},
		{"chore: tidy\n\nBREAKING-CHANGE: config renamed", Major},
		{"Implement R1 milestones M0 to M2", Patch}, // not conventional: never silently dropped
		{"Merge pull request #1 from x/y", Patch},
		{"feat:missing space", Patch},
		{"fix: mention BREAKING CHANGE: in the subject only", Patch},
		{"   ", None},
	}
	for _, tc := range cases {
		if got := Classify(tc.msg); got != tc.want {
			t.Errorf("Classify(%q) = %d, want %d", tc.msg, got, tc.want)
		}
	}
}

func TestNext(t *testing.T) {
	cases := []struct {
		name     string
		last     string
		messages []string
		want     string
	}{
		{"first release", "", []string{"Implement M0"}, "v0.1.0"},
		{"first release from a feature", "", []string{"feat: x"}, "v0.1.0"},
		{"nothing before the first release", "", []string{"docs: x"}, ""},
		{"no commits", "v0.1.0", nil, ""},
		{"only non-releasing commits", "v0.1.0", []string{"docs: a", "ci: b"}, ""},
		{"patch", "v0.1.0", []string{"fix: a", "docs: b"}, "v0.1.1"},
		{"minor wins over patch", "v0.1.3", []string{"fix: a", "feat: b"}, "v0.2.0"},
		{"breaking before 1.0 is minor", "v0.4.2", []string{"feat!: a"}, "v0.5.0"},
		{"breaking after 1.0 is major", "v1.4.2", []string{"fix: a", "feat!: b"}, "v2.0.0"},
		{"feature after 1.0", "v1.4.2", []string{"feat: a"}, "v1.5.0"},
		{"multi-digit numbers", "v0.9.19", []string{"fix: a"}, "v0.9.20"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Next(tc.last, tc.messages)
			if err != nil || got != tc.want {
				t.Errorf("Next = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
	for _, bad := range []string{"0.1.0", "v1.2", "v1.2.3-rc.1", "v01.2.3", "latest"} {
		if _, err := Next(bad, []string{"fix: a"}); err == nil {
			t.Errorf("Next(%q) accepted a malformed tag", bad)
		}
	}
}

func TestReadMessages(t *testing.T) {
	in := "feat: a\n\nbody line\x00\n\x00fix: b\x00docs: c"
	got, err := readMessages(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"feat: a\n\nbody line", "fix: b", "docs: c"}
	if len(got) != len(want) {
		t.Fatalf("got %q", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("message %d = %q, want %q", i, got[i], want[i])
		}
	}
}
