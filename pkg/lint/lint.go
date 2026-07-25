// Package lint surfaces docs-hygiene issues the link graph proper doesn't.
// Two checks: doc references written as inline code (`docs/design.md`) rather
// than as [text](path) links — which never become graph edges, so a doc only
// ever pointed at that way reads as an orphan — split into unlinked references
// (resolve to a file; turning them into links enriches the graph) and broken
// inline references (resolve to nothing); and deep relative links, real
// [text](path) links that climb several directories with `../`, which a
// root-absolute path (`/docs/x.md`) states more stably.
package lint

import (
	"path"
	"sort"
	"strings"

	"github.com/reactor-team/semantic/pkg/chunk"
	"github.com/reactor-team/semantic/pkg/graph"
	"github.com/reactor-team/semantic/pkg/index"
	"github.com/reactor-team/semantic/pkg/toc"
)

// DeepLinkClimb is the number of parent-directory hops ('../') at which a
// relative link is flagged in favor of a root-absolute path.
const DeepLinkClimb = 2

// Source is the read side of the index lint needs. *index.Store satisfies it.
type Source interface {
	AllFiles() ([]string, error)
	AllLinks() ([]index.LinkRow, error)
	FileHeadings() (map[string]map[string]bool, error)
}

// Ref is one flagged reference — an inline-code doc path or a deep relative
// link — and where it points.
type Ref struct {
	From       string   `json:"from"`
	Raw        string   `json:"target"`               // the path as written
	Anchor     string   `json:"anchor,omitempty"`     // #section fragment, if any
	To         string   `json:"to,omitempty"`         // resolved rel-path; empty = unresolved
	Suggest    string   `json:"suggest,omitempty"`    // root-absolute rewrite (deep links only)
	Candidates []string `json:"candidates,omitempty"` // ambiguous refs only: every same-named file it could mean
	Line       int      `json:"line"`
}

// Report groups the flagged references by kind.
type Report struct {
	Unlinked     []Ref        `json:"unlinked"`      // resolve to an indexed doc — should be links
	Ambiguous    []Ref        `json:"ambiguous"`     // bare basename matches >1 indexed file — needs a fuller path
	Broken       []Ref        `json:"broken"`        // resolve to nothing — dead inline paths
	DeepRelative []Ref        `json:"deep_relative"` // real links climbing ≥DeepLinkClimb dirs — prefer root-absolute
	MissingTOC   []TOCFinding `json:"missing_toc"`   // long markdown files with no current Contents TOC
}

// Analyze loads the indexed files and link records and classifies them. Every
// inline-code doc-path reference (LinkCode) is unlinked only if it fully
// resolves — the path finds a file and any #section fragment finds a heading in
// it; a missing file or section makes it broken. A bare basename (no
// directory) matching more than one indexed file of the same extension is
// ambiguous instead: Resolver.Resolve's first-match pick would be arbitrary,
// so it's reported with its full candidate list rather than silently promoted
// to a possibly-wrong file. Every real markdown link (LinkMarkdown) that
// climbs DeepLinkClimb+ directories and stays within the tree is flagged
// deep-relative, with a root-absolute rewrite.
//
// linkRoot is the vault's forward-slashed path within its enclosing repository
// ("" when the vault is the repo root). A leading-'/' link resolves against the
// repo root, so the rewrite is anchored there rather than at the vault — a
// vault indexed below the repo root ("subproj") gets that prefix, keeping the
// suggestion pointed at the intended file.
func Analyze(src Source, linkRoot string) (*Report, error) {
	files, err := src.AllFiles()
	if err != nil {
		return nil, err
	}
	links, err := src.AllLinks()
	if err != nil {
		return nil, err
	}
	headings, err := src.FileHeadings()
	if err != nil {
		return nil, err
	}

	r := graph.NewResolver(files)
	rep := &Report{}
	for _, l := range links {
		switch l.Kind {
		case chunk.LinkCode:
			ref := Ref{
				From:   l.SrcRelPath,
				Raw:    l.Target,
				Anchor: l.Anchor,
				Line:   l.Line,
			}
			if cands := r.Candidates(l.Target); len(cands) > 1 {
				ref.Candidates = cands
				rep.Ambiguous = append(rep.Ambiguous, ref)
				continue
			}
			ref.To = r.Resolve(l.SrcRelPath, l.Target, l.Kind)
			anchorOK := ref.Anchor == "" || headings[ref.To][chunk.AnchorSlug(ref.Anchor)]
			if ref.To != "" && anchorOK {
				rep.Unlinked = append(rep.Unlinked, ref)
			} else {
				rep.Broken = append(rep.Broken, ref)
			}
		case chunk.LinkMarkdown:
			if climbDepth(l.Target) < DeepLinkClimb {
				continue
			}
			cleaned := path.Clean(path.Join(path.Dir(l.SrcRelPath), l.Target))
			if strings.HasPrefix(cleaned, "..") {
				continue // climbs out of the tree — no root-absolute form to suggest
			}
			rep.DeepRelative = append(rep.DeepRelative, Ref{
				From:    l.SrcRelPath,
				Raw:     l.Target,
				Anchor:  l.Anchor,
				To:      r.Resolve(l.SrcRelPath, l.Target, l.Kind),
				Suggest: rootAbsolute(linkRoot, cleaned, l.Anchor),
				Line:    l.Line,
			})
		}
	}
	sortRefs(rep.Unlinked)
	sortRefs(rep.Ambiguous)
	sortRefs(rep.Broken)
	sortRefs(rep.DeepRelative)
	return rep, nil
}

// climbDepth counts the leading parent-directory hops in a relative link
// target after cleaning ("../../setup/x.md" → 2). Root-absolute and
// non-climbing targets are 0.
func climbDepth(target string) int {
	if target == "" || strings.HasPrefix(target, "/") {
		return 0
	}
	n := 0
	for seg := range strings.SplitSeq(path.Clean(target), "/") {
		if seg != ".." {
			break
		}
		n++
	}
	return n
}

// rootAbsolute renders a cleaned within-tree path as a root-absolute link,
// prefixed with the vault's path inside the repo (linkRoot) so it resolves
// against the repo root, and re-attaching the #section anchor for a
// copy-pasteable suggestion.
func rootAbsolute(linkRoot, cleaned, anchor string) string {
	if linkRoot != "" {
		cleaned = path.Join(linkRoot, cleaned)
	}
	s := "/" + cleaned
	if anchor != "" {
		s += "#" + anchor
	}
	return s
}

// DeepFix is one applied deep-link rewrite: the destination as written, its
// root-absolute replacement, and how many occurrences were rewritten.
type DeepFix struct {
	From      string `json:"from"`
	OldTarget string `json:"old_target"`
	NewTarget string `json:"new_target"`
	Count     int    `json:"count"`
}

// DestWithAnchor recombines the destination as written: Raw, plus "#Anchor"
// when the reference carried a #section fragment.
func (r *Ref) DestWithAnchor() string {
	if r.Anchor == "" {
		return r.Raw
	}
	return r.Raw + "#" + r.Anchor
}

// applyLineFixes rewrites refs in content one flagged occurrence at a time.
// For each ref it recombines the destination as written and calls rewrite to
// produce the replacement for that ref's own line; rewrite reports false when
// the destination isn't found verbatim there, so the ref is skipped rather
// than guessed (an identical span in a code fence or example elsewhere is
// never touched, since only the flagged line is edited). Returns the new
// content and the count of rewrites per destination-as-written.
func applyLineFixes(content string, refs []Ref, rewrite func(line, dest string, r Ref) (string, bool)) (fixed string, counts map[string]int) {
	lines := strings.Split(content, "\n")
	counts = map[string]int{}
	for _, r := range refs {
		idx := r.Line - 1
		if idx < 0 || idx >= len(lines) {
			continue
		}
		dest := r.DestWithAnchor()
		newLine, ok := rewrite(lines[idx], dest, r)
		if !ok {
			continue
		}
		lines[idx] = newLine
		counts[dest]++
	}
	return strings.Join(lines, "\n"), counts
}

// ApplyDeepFixes rewrites deep relative links in one file's content to their
// root-absolute form, replacing each `](target)` with `](suggestion)`. refs
// must be the DeepRelative refs for that file, one per flagged occurrence —
// each is rewritten only on its own Ref.Line, never by searching the whole
// file for matching text, so an identical span sitting in a code fence or
// illustrative example elsewhere in the file (never itself flagged by
// Analyze) is left alone. A destination not found verbatim on that line — a
// link with a title, a ?query, or already-edited text — is skipped, so the
// rewrite never guesses; only exact `](dest)` spans are touched. Returns the
// new content and the fixes applied.
func ApplyDeepFixes(content string, refs []Ref) (string, []DeepFix) {
	if len(refs) == 0 {
		return content, nil
	}
	newDestOf := map[string]string{} // destination-as-written → root-absolute
	out, counts := applyLineFixes(content, refs, func(line, dest string, r Ref) (string, bool) {
		newDestOf[dest] = r.Suggest
		newLine := strings.Replace(line, "]("+dest+")", "]("+r.Suggest+")", 1)
		return newLine, newLine != line
	})
	var fixes []DeepFix
	for oldDest, n := range counts {
		fixes = append(fixes, DeepFix{From: refs[0].From, OldTarget: oldDest, NewTarget: newDestOf[oldDest], Count: n})
	}
	sort.Slice(fixes, func(i, j int) bool { return fixes[i].OldTarget < fixes[j].OldTarget })
	return out, fixes
}

// UnlinkedFix is one applied "make it a real link" rewrite: an inline-code
// path promoted to a markdown link, keeping the original text as the link's
// code-span label and pointing at a root-absolute target.
type UnlinkedFix struct {
	From   string `json:"from"`
	Target string `json:"target"` // the inline-code destination as written (path[#anchor])
	Link   string `json:"link"`   // root-absolute href written into the new link
	Count  int    `json:"count"`
}

// ApplyUnlinkedFixes rewrites each Unlinked ref's inline-code span
// (“ `dest` “) to a real markdown link (“ [`dest`](/link) “), turning a bare
// path mention into a link the graph can traverse.
//
// The label keeps the original text verbatim rather than shortening it to a
// basename: a shortened label reads as ambiguous the moment two refs in the
// same file resolve to same-named files in different directories (two
// `main.go`s). The href is root-absolute rather than the raw destination:
// an Unlinked ref's raw text resolves vault-relative — like a wikilink, see
// Resolver.resolveWiki — which is not generally a valid same-directory
// relative link from wherever the reference happens to be written; a nested
// file writing “ `pkg/x.go` “ verbatim would produce a link GitHub
// resolves relative to that file's own directory, landing somewhere the path
// never meant to reach. Anchoring at the repo root the way GitHub follows a
// leading-'/' link keeps the promoted link correct regardless of where the
// reference lives — the same fix DeepFix already applies to deep-relative
// real links. linkRoot is the vault's path within its enclosing repo, as in
// Analyze.
//
// refs must be the Unlinked refs for that file, one per flagged occurrence —
// each is rewritten only on its own Ref.Line, never by searching the whole
// file for matching text, so an identical span sitting in a code fence, an
// ignored placeholder, or another literal example elsewhere in the file
// (never itself flagged by Analyze) is left alone. A destination not found
// verbatim as a code span on that line (a stray ?query, already-edited
// text) is skipped, so the rewrite never guesses; only exact “ `dest` “
// spans are touched.
func ApplyUnlinkedFixes(content string, refs []Ref, linkRoot string) (string, []UnlinkedFix) {
	if len(refs) == 0 {
		return content, nil
	}
	linkOf := map[string]string{} // destination-as-written → root-absolute href
	out, counts := applyLineFixes(content, refs, func(line, dest string, r Ref) (string, bool) {
		target := rootAbsolute(linkRoot, r.To, r.Anchor)
		linkOf[dest] = target
		return replaceOneBareCodeSpan(line, "`"+dest+"`", "[`"+dest+"`]("+target+")")
	})
	var fixes []UnlinkedFix
	for dest, n := range counts {
		fixes = append(fixes, UnlinkedFix{From: refs[0].From, Target: dest, Link: linkOf[dest], Count: n})
	}
	sort.Slice(fixes, func(i, j int) bool { return fixes[i].Target < fixes[j].Target })
	return out, fixes
}

// replaceOneBareCodeSpan finds the first occurrence of a code span (“ `dest`
// “) in line that isn't already a link's label — immediately preceded by '['
// and followed by "](" — and replaces just that one, returning the new line
// and true. Returns line unchanged and false if every occurrence is already
// linked, or there is none. Scoped to a single line (see ApplyUnlinkedFixes),
// so multiple flagged occurrences on the same line are consumed one call at
// a time, each finding the next still-bare span left by the previous call.
func replaceOneBareCodeSpan(line, oldSpan, newLink string) (string, bool) {
	offset := 0
	for {
		i := strings.Index(line[offset:], oldSpan)
		if i < 0 {
			return line, false
		}
		abs := offset + i
		before, after := line[:abs], line[abs+len(oldSpan):]
		if strings.HasSuffix(before, "[") && strings.HasPrefix(after, "](") {
			offset = abs + len(oldSpan) // already a link's label — keep scanning past it
			continue
		}
		return before + newLink + after, true
	}
}

// TOCFinding is one markdown file over toc.LineThreshold lines whose committed
// Contents TOC is missing or stale. Reason is "missing" (no up-to-date block)
// or "stale" (a block exists but no longer matches the headings).
type TOCFinding struct {
	File   string `json:"file"`
	Lines  int    `json:"lines"`
	Reason string `json:"reason"`
}

// AuditTOCs flags long markdown files lacking a current Contents TOC. A file is
// in scope when it is markdown, exceeds toc.LineThreshold lines, has at least
// one heading a TOC would list, and does not opt out with
// `<!-- semantic-ignore-file -->`; read supplies file content by rel-path.
// A file whose content can't be read is skipped rather than failing the audit.
func AuditTOCs(files []string, read func(string) (string, error)) ([]TOCFinding, error) {
	var out []TOCFinding
	for _, f := range files {
		if !chunk.IsMarkdown(f) {
			continue
		}
		content, err := read(f)
		if err != nil {
			continue
		}
		if chunk.IgnoresFile(content) {
			continue
		}
		a := toc.Inspect(content)
		if a.Lines <= toc.LineThreshold || a.Entries == 0 {
			continue
		}
		if a.HasBlock && a.UpToDate {
			continue
		}
		reason := "missing"
		if a.HasBlock {
			reason = "stale"
		}
		out = append(out, TOCFinding{File: f, Lines: a.Lines, Reason: reason})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].File < out[j].File })
	return out, nil
}

// KeepFiles returns the refs sourced from one of files — a set of
// vault-relative, forward-slashed rel-paths — narrowing a whole-vault report to
// a chosen set (e.g. the staged files a pre-commit hook passes). A nil/empty set
// keeps nothing.
func KeepFiles(refs []Ref, files map[string]bool) []Ref {
	var out []Ref
	for _, r := range refs {
		if files[r.From] {
			out = append(out, r)
		}
	}
	return out
}

// sortRefs orders refs by source path then line for a stable report.
func sortRefs(refs []Ref) {
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].From != refs[j].From {
			return refs[i].From < refs[j].From
		}
		return refs[i].Line < refs[j].Line
	})
}
