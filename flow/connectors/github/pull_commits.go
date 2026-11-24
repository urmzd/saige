package github

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
)

// PullRequestCommitsAPI reads the commits of a pull request, so a host can
// refuse an approval asked for by someone who wrote, committed or co-authored
// one of them. The model has
// no tool for it.
type PullRequestCommitsAPI interface {
	PullRequestCommits(ctx context.Context, owner, repo string, number int) ([]PRCommit, error)
}

// PRCommit is one commit of a pull request: the GitHub accounts linked to its
// author and committer, the git author and committer emails, the people its
// message names as co-authors, and GitHub's verdict on its signature.
//
// An ID of 0 is unknown and matches nobody: GitHub links no account, such as
// for a commit made with an email no account claims. A host that refuses an
// approval asked for by someone who wrote the change denies a requested
// approval when any commit has an author or committer it cannot map to a
// person: an ID of 0, or an ID, email or co-author it does not know. GitHub
// commits merges, squashes and web edits as WebFlowLogin; CommittedByWebFlow
// tells such a committer apart, which a host may ignore as committer while it
// still checks the author.
type PRCommit struct {
	SHA            string `json:"sha"`
	AuthorID       int64  `json:"author_id,omitempty"`
	CommitterID    int64  `json:"committer_id,omitempty"`
	AuthorLogin    string `json:"author_login,omitempty"`
	CommitterLogin string `json:"committer_login,omitempty"`
	// AuthorEmail and CommitterEmail are the emails the git commit records,
	// whether or not an account claims them.
	AuthorEmail    string `json:"author_email,omitempty"`
	CommitterEmail string `json:"committer_email,omitempty"`
	// CoAuthors are the Co-authored-by trailers of the commit message, in
	// order, at most MaxCoAuthors.
	CoAuthors []CoAuthor `json:"co_authors,omitempty"`
	// Verification is GitHub's verdict on the commit's signature.
	Verification CommitVerification `json:"verification,omitzero"`
}

// CommitVerification is GitHub's verdict on a commit's signature: Verified,
// and Reason, such as "valid", "unsigned" or "unknown_key". The author,
// committer and co-authors of a commit are whatever its maker wrote; only a
// verified signature ties a commit to a key its account holds. A host that
// needs proof of who wrote a change requires every commit to be verified.
// GitHub's REST response names no signer, so none is read.
type CommitVerification struct {
	Verified bool   `json:"verified"`
	Reason   string `json:"reason,omitempty"`
}

// CoAuthor is one "Co-authored-by: Name <email>" line of a commit message.
// Email is empty when the line names no address in angle brackets; Name is
// then the rest of the line.
type CoAuthor struct {
	Name  string `json:"name,omitempty"`
	Email string `json:"email,omitempty"`
}

// The account GitHub commits merges, squashes and web edits as.
const (
	WebFlowLogin       = "web-flow"
	WebFlowID    int64 = 19864447
)

// CommittedByWebFlow reports whether GitHub's web-flow account committed the
// commit, by both its ID and its login.
func (c PRCommit) CommittedByWebFlow() bool {
	return c.CommitterID == WebFlowID && strings.EqualFold(c.CommitterLogin, WebFlowLogin)
}

// MaxCoAuthors bounds the co-authors read from one commit message. A commit
// that names more fails the read rather than returning a part of them.
const MaxCoAuthors = 50

// coAuthorKey is the trailer key of a co-author line, compared ignoring case.
const coAuthorKey = "co-authored-by"

// coAuthorAddress is an address in angle brackets that holds no other bracket.
var coAuthorAddress = regexp.MustCompile(`<([^<>]*)>`)

// coAuthors reads every line of message whose key is Co-authored-by, ignoring
// case and any space or tab before and after the colon. A line anywhere in the
// message counts, not only in the final trailer block, and a line ends at
// "\n", "\r\n" or a lone "\r". A line names one co-author for each part
// between commas outside angle brackets, so "Doe, Jo <jo@example.com>" names
// "Doe" without an email and "Jo". The email of a part is its first address
// in angle brackets that holds no other bracket, whatever follows it: the
// innermost one, so "Name<<a@example.com>>" gives a@example.com. It fails past
// MaxCoAuthors.
func coAuthors(message string) ([]CoAuthor, error) {
	message = strings.ReplaceAll(message, "\r\n", "\n")
	message = strings.ReplaceAll(message, "\r", "\n")
	var out []CoAuthor
	for _, line := range strings.Split(message, "\n") {
		line = strings.TrimSpace(line)
		if len(line) < len(coAuthorKey) || !strings.EqualFold(line[:len(coAuthorKey)], coAuthorKey) {
			continue
		}
		rest := strings.TrimLeft(line[len(coAuthorKey):], " \t")
		if !strings.HasPrefix(rest, ":") {
			continue
		}
		for _, part := range splitOutsideBrackets(rest[1:]) {
			if len(out) == MaxCoAuthors {
				return nil, fmt.Errorf("github: commit names more than %d co-authors", MaxCoAuthors)
			}
			out = append(out, coAuthorOf(part))
		}
	}
	return out, nil
}

// coAuthorOf reads one co-author: the text before the address is the name.
func coAuthorOf(part string) CoAuthor {
	loc := coAuthorAddress.FindStringSubmatchIndex(part)
	if loc == nil {
		return CoAuthor{Name: part}
	}
	return CoAuthor{Name: strings.TrimSpace(strings.TrimRight(part[:loc[0]], "< \t")), Email: strings.TrimSpace(part[loc[2]:loc[3]])}
}

// splitOutsideBrackets splits value at commas outside angle brackets, trims
// each part, and drops empty ones.
func splitOutsideBrackets(value string) []string {
	var parts []string
	depth, start := 0, 0
	for i, r := range value {
		switch r {
		case '<':
			depth++
		case '>':
			depth = max(depth-1, 0)
		case ',':
			if depth == 0 {
				parts = append(parts, value[start:i])
				start = i + 1
			}
		}
	}
	parts = append(parts, value[start:])
	out := parts[:0]
	for _, part := range parts {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// MaxPullRequestCommits is how many commits GitHub lists for a pull request.
// A read that reaches it fails: the list may be cut short, and a host that
// checks who wrote the commits cannot tell.
const MaxPullRequestCommits = 250

const pullCommitsPage = 100

// PullRequestCommits reads every commit of a pull request, following pages of
// 100 until one comes back short. It fails when the pull request has
// MaxPullRequestCommits or more commits, or a commit names more than
// MaxCoAuthors co-authors. An ID GitHub reports below 1 is read as 0.
func (a *HTTPAPI) PullRequestCommits(ctx context.Context, owner, repo string, number int) ([]PRCommit, error) {
	ref := Ref{Owner: owner, Repo: repo, Number: number}
	if err := validReadRef(ref, 1); err != nil {
		return nil, err
	}
	type account struct {
		Login string `json:"login"`
		ID    int64  `json:"id"`
	}
	type person struct {
		Email string `json:"email"`
	}
	var out []PRCommit
	for page := 1; ; page++ {
		var rows []struct {
			SHA    string `json:"sha"`
			Commit struct {
				Message      string             `json:"message"`
				Author       *person            `json:"author"`
				Committer    *person            `json:"committer"`
				Verification CommitVerification `json:"verification"`
			} `json:"commit"`
			Author    *account `json:"author"`
			Committer *account `json:"committer"`
		}
		path := fmt.Sprintf("/repos/%s/%s/pulls/%d/commits?per_page=%d&page=%d", owner, repo, number, pullCommitsPage, page)
		if err := a.client.Do(ctx, http.MethodGet, path, nil, &rows); err != nil {
			return nil, fmt.Errorf("github: list commits of %s: %w", ref, err)
		}
		for _, row := range rows {
			if !validSHA(row.SHA) {
				return nil, fmt.Errorf("github: commit list of %s has an invalid SHA", ref)
			}
			c := PRCommit{SHA: row.SHA, Verification: row.Commit.Verification}
			if row.Author != nil {
				c.AuthorID, c.AuthorLogin = knownID(row.Author.ID), row.Author.Login
			}
			if row.Committer != nil {
				c.CommitterID, c.CommitterLogin = knownID(row.Committer.ID), row.Committer.Login
			}
			if row.Commit.Author != nil {
				c.AuthorEmail = row.Commit.Author.Email
			}
			if row.Commit.Committer != nil {
				c.CommitterEmail = row.Commit.Committer.Email
			}
			people, err := coAuthors(row.Commit.Message)
			if err != nil {
				return nil, fmt.Errorf("%w in %s", err, ref)
			}
			c.CoAuthors = people
			out = append(out, c)
		}
		if len(out) >= MaxPullRequestCommits {
			return nil, fmt.Errorf("github: %s has %d or more commits", ref, MaxPullRequestCommits)
		}
		if len(rows) < pullCommitsPage {
			return out, nil
		}
	}
}

// SeedPullRequestCommits sets the commits of a pull request in the fake. An ID
// below 1 is read as 0, as the HTTP API reads it.
func (f *Fake) SeedPullRequestCommits(ref Ref, commits []PRCommit) error {
	if err := validReadRef(ref, 1); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.pullCommits == nil {
		f.pullCommits = make(map[string][]PRCommit)
	}
	seeded := make([]PRCommit, len(commits))
	for i, c := range commits {
		c.AuthorID, c.CommitterID = knownID(c.AuthorID), knownID(c.CommitterID)
		c.CoAuthors = slices.Clone(c.CoAuthors)
		seeded[i] = c
	}
	f.pullCommits[pullCommitsKey(ref)] = seeded
	return nil
}

// PullRequestCommits returns the seeded commits, and fails at
// MaxPullRequestCommits as the HTTP API does.
func (f *Fake) PullRequestCommits(_ context.Context, owner, repo string, number int) ([]PRCommit, error) {
	ref := Ref{Owner: owner, Repo: repo, Number: number}
	if err := validReadRef(ref, 1); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return nil, f.Err
	}
	commits := f.pullCommits[pullCommitsKey(ref)]
	if len(commits) >= MaxPullRequestCommits {
		return nil, fmt.Errorf("github: %s has %d or more commits", ref, MaxPullRequestCommits)
	}
	return append([]PRCommit(nil), commits...), nil
}

func pullCommitsKey(ref Ref) string { return workflowKey(ref) + "#" + fmt.Sprint(ref.Number) }

var (
	_ PullRequestCommitsAPI = (*HTTPAPI)(nil)
	_ PullRequestCommitsAPI = (*Fake)(nil)
)
