// Command nextversion works out the next release version from commit
// messages, using Conventional Commits. See docs/releasing.md.
//
// It reads commit messages from stdin, each terminated by a NUL byte (git log
// --format='%B%x00'), and takes the latest release tag with -last. It prints
// the next version, such as v0.4.0, or nothing when the commits do not
// warrant a release.
package main

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// Initial is the first release version.
const Initial = "v0.1.0"

// Bump is the size of a version change.
type Bump int

const (
	None Bump = iota
	Patch
	Minor
	Major
)

// headerRe matches a Conventional Commits header: type(scope)!: subject.
var headerRe = regexp.MustCompile(`^([a-zA-Z]+)(\([^)]*\))?(!)?:\s`)

// nonReleasing types change nothing a user of a release would notice.
var nonReleasing = map[string]bool{
	"docs": true, "chore": true, "ci": true, "test": true, "style": true, "build": true,
}

// Classify returns the bump one commit message calls for. A message that
// does not follow the convention is treated as a patch, so a change is never
// silently left out of a release.
func Classify(message string) Bump {
	message = strings.TrimSpace(message)
	if message == "" {
		return None
	}
	header, body, _ := strings.Cut(message, "\n")
	if strings.Contains("\n"+body, "\nBREAKING CHANGE:") || strings.Contains("\n"+body, "\nBREAKING-CHANGE:") {
		return Major
	}
	m := headerRe.FindStringSubmatch(header)
	if m == nil {
		return Patch
	}
	if m[3] == "!" {
		return Major
	}
	switch kind := strings.ToLower(m[1]); {
	case kind == "feat":
		return Minor
	case nonReleasing[kind]:
		return None
	default:
		return Patch
	}
}

// Next returns the version after last for the given commits, or "" when no
// release is due. last is "" before the first release.
func Next(last string, messages []string) (string, error) {
	bump := None
	for _, m := range messages {
		bump = max(bump, Classify(m))
	}
	if last == "" {
		if bump == None {
			return "", nil
		}
		return Initial, nil
	}
	major, minor, patch, err := parse(last)
	if err != nil {
		return "", err
	}
	// Before 1.0 the API is not stable: a breaking change moves the minor
	// number, and 1.0.0 is cut deliberately, not by a commit message.
	if major == 0 && bump == Major {
		bump = Minor
	}
	switch bump {
	case Major:
		return fmt.Sprintf("v%d.0.0", major+1), nil
	case Minor:
		return fmt.Sprintf("v%d.%d.0", major, minor+1), nil
	case Patch:
		return fmt.Sprintf("v%d.%d.%d", major, minor, patch+1), nil
	}
	return "", nil
}

var versionRe = regexp.MustCompile(`^v(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)$`)

func parse(v string) (major, minor, patch int, err error) {
	m := versionRe.FindStringSubmatch(v)
	if m == nil {
		return 0, 0, 0, fmt.Errorf("nextversion: %q is not a release tag like v1.2.3", v)
	}
	major, _ = strconv.Atoi(m[1])
	minor, _ = strconv.Atoi(m[2])
	patch, _ = strconv.Atoi(m[3])
	return major, minor, patch, nil
}

// readMessages splits NUL-terminated commit messages.
func readMessages(r io.Reader) ([]string, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
	sc.Split(func(data []byte, atEOF bool) (int, []byte, error) {
		if i := bytes.IndexByte(data, 0); i >= 0 {
			return i + 1, data[:i], nil
		}
		if atEOF && len(data) > 0 {
			return len(data), data, nil
		}
		return 0, nil, nil
	})
	var out []string
	for sc.Scan() {
		if m := strings.TrimSpace(sc.Text()); m != "" {
			out = append(out, m)
		}
	}
	return out, sc.Err()
}

func main() {
	last := flag.String("last", "", "latest release tag; empty before the first release")
	flag.Parse()

	messages, err := readMessages(os.Stdin)
	if err == nil {
		var next string
		if next, err = Next(strings.TrimSpace(*last), messages); err == nil {
			if next != "" {
				fmt.Println(next)
			}
			return
		}
	}
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
