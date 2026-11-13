package indexer

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
)

// Entry is one timestamped block of a journal file.
type Entry struct {
	Date    string // YYYY-MM-DD
	Time    string // HH:MM, or "" when the block has no time heading
	Heading string // heading text after the time, tags removed
	Body    string // org text below the heading, property drawer removed
	Tags    []string
	Props   map[string]string
	Words   int
	Hash    string // SHA-256 of Body; grades are keyed by this
}

var (
	journalName = regexp.MustCompile(`^(\d{4})(\d{2})(\d{2})\.org$`)
	level2      = regexp.MustCompile(`^\*\*\s+(.*)$`)
	timePrefix  = regexp.MustCompile(`^(\d{1,2}:\d{2})\b\s*(.*)$`)
	tagSuffix   = regexp.MustCompile(`\s+:([\w@#%:]+):\s*$`)
	propLine    = regexp.MustCompile(`^\s*:([^:\s]+):\s*(.*)$`)
)

// JournalDate returns the date encoded in a journal file name such as 20261113.org.
func JournalDate(base string) (string, bool) {
	m := journalName.FindStringSubmatch(base)
	if m == nil {
		return "", false
	}
	return m[1] + "-" + m[2] + "-" + m[3], true
}

// SplitJournal splits one day's file into entries. Each level-2 heading starts
// an entry; text before the first one becomes an entry with no time. The
// level-1 day heading and file keywords (#+title: ...) are dropped.
func SplitJournal(date, src string) []Entry {
	var out []Entry
	cur := Entry{Date: date}
	var body []string
	inProps, started := false, false

	flush := func() {
		cur.Body = strings.TrimSpace(strings.Join(body, "\n"))
		if cur.Body != "" {
			sum := sha256.Sum256([]byte(cur.Body))
			cur.Hash = hex.EncodeToString(sum[:])
			cur.Words = len(strings.Fields(cur.Body))
			out = append(out, cur)
		}
		body = nil
	}

	for _, line := range strings.Split(strings.ReplaceAll(src, "\r\n", "\n"), "\n") {
		if m := level2.FindStringSubmatch(line); m != nil {
			flush()
			cur = Entry{Date: date}
			started, inProps = true, false
			head := m[1]
			if t := tagSuffix.FindStringSubmatch(head); t != nil {
				cur.Tags = strings.Split(t[1], ":")
				head = strings.TrimSpace(tagSuffix.ReplaceAllString(head, ""))
			}
			if t := timePrefix.FindStringSubmatch(head); t != nil {
				cur.Time = t[1]
				if len(cur.Time) == 4 {
					cur.Time = "0" + cur.Time
				}
				head = t[2]
			}
			cur.Heading = strings.TrimSpace(head)
			continue
		}
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "* "): // the day heading
			continue
		case !started && strings.HasPrefix(trimmed, "#+"):
			continue
		case strings.EqualFold(trimmed, ":PROPERTIES:") && len(strings.TrimSpace(strings.Join(body, ""))) == 0:
			inProps = true
			continue
		case inProps && strings.EqualFold(trimmed, ":END:"):
			inProps = false
			continue
		case inProps:
			if p := propLine.FindStringSubmatch(line); p != nil {
				if cur.Props == nil {
					cur.Props = map[string]string{}
				}
				cur.Props[p[1]] = strings.TrimSpace(p[2])
			}
			continue
		}
		body = append(body, line)
	}
	flush()
	return out
}
