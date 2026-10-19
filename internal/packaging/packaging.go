//go:build !js

package packaging

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// The artefact names this package checks. They are relative to the repository
// root so that no check has to know where the checkout lives.
const (
	modulePath   = "github.com/gjbrr/blake-swap"
	goModName    = "go.mod"
	goSumName    = "go.sum"
	makefileName = "Makefile"
	licenseName  = "LICENSE"
	ciWorkflow   = ".github/workflows/ci.yml"
)

// FindRoot walks up from start until it finds the directory that holds go.mod.
// It returns an error at the filesystem root rather than guessing, so a test
// run from outside a checkout fails loudly instead of checking nothing.
func FindRoot(start string) (string, error) {
	dir, err := filepath.Abs(start)
	if err != nil {
		return "", fmt.Errorf("resolve %q: %w", start, err)
	}

	for {
		info, statErr := os.Stat(filepath.Join(dir, goModName))

		switch {
		case statErr == nil && !info.IsDir():
			return dir, nil
		case statErr != nil && !errors.Is(statErr, fs.ErrNotExist):
			return "", fmt.Errorf("stat %s: %w", filepath.Join(dir, goModName), statErr)
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no %s found from %q up to the filesystem root", goModName, start)
		}

		dir = parent
	}
}

// CheckGoMod verifies that go.mod parses, declares this repository's module
// path and carries a go directive.
//
// golang.org/x/mod/modfile is not a dependency of this module, and adding one
// to check go.mod would make the check mutate the thing it checks. The two
// fields this invariant needs are therefore read by a small, strict parser:
// only single-line module and go directives are accepted, duplicates are an
// error, and the go version must look like a Go version. Everything else in
// the file is left to the toolchain.
func CheckGoMod(root string) error {
	data, err := os.ReadFile(filepath.Join(root, goModName))
	if err != nil {
		return fmt.Errorf("read %s: %w", goModName, err)
	}

	module, goVersion, err := parseGoMod(string(data))
	if err != nil {
		return fmt.Errorf("%s: %w", goModName, err)
	}

	if module != modulePath {
		return fmt.Errorf("%s declares module %q, want %q", goModName, module, modulePath)
	}

	if goVersion == "" {
		return fmt.Errorf("%s has no go directive", goModName)
	}

	return nil
}

// goDirectiveRE matches the version forms the go directive accepts here. A
// patch release is optional: both "1.25" and "1.25.3" are valid directives.
var goDirectiveRE = regexp.MustCompile(`^1\.\d+(?:\.\d+)?$`)

// parseGoMod reads the module path and go directive out of go.mod. It is
// deliberately strict about the two fields it understands: a malformed or
// duplicated directive is an error, not a silently ignored line.
func parseGoMod(text string) (module, goVersion string, err error) {
	for index, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		lineNumber := index + 1

		if line == "" || strings.HasPrefix(line, "//") {
			continue
		}

		fields := strings.Fields(line)

		switch fields[0] {
		case "module":
			if module != "" {
				return "", "", fmt.Errorf("line %d: duplicate module directive", lineNumber)
			}

			if len(fields) != 2 {
				return "", "", fmt.Errorf(
					"line %d: module directive must be a single path, found %d fields",
					lineNumber, len(fields),
				)
			}

			value := fields[1]
			if strings.HasPrefix(value, `"`) {
				unquoted, unquoteErr := strconv.Unquote(value)
				if unquoteErr != nil {
					return "", "", fmt.Errorf("line %d: module path is not a valid quoted string: %w", lineNumber, unquoteErr)
				}

				value = unquoted
			}

			if value == "" {
				return "", "", fmt.Errorf("line %d: module path is empty", lineNumber)
			}

			module = value

		case "go":
			if goVersion != "" {
				return "", "", fmt.Errorf("line %d: duplicate go directive", lineNumber)
			}

			if len(fields) != 2 {
				return "", "", fmt.Errorf(
					"line %d: go directive must be a single version, found %d fields",
					lineNumber, len(fields),
				)
			}

			if !goDirectiveRE.MatchString(fields[1]) {
				return "", "", fmt.Errorf("line %d: go directive %q is not a Go version", lineNumber, fields[1])
			}

			goVersion = fields[1]
		}
	}

	if module == "" {
		return "", "", errors.New("no module directive")
	}

	if goVersion == "" {
		return "", "", errors.New("no go directive")
	}

	return module, goVersion, nil
}

// CheckGoSum verifies that go.sum exists, is non-empty, and that every
// non-comment line has the shape go writes:
//
//	<module> <version> <hash>
//	<module> <version>/go.mod <hash>
//
// where the hash is an h1: SHA-256 digest. A truncated download, an editor
// that dropped a column, or an unsupported hash algorithm is therefore a
// failure rather than something the toolchain discovers mid-build.
func CheckGoSum(root string) error {
	data, err := os.ReadFile(filepath.Join(root, goSumName))
	if err != nil {
		return fmt.Errorf("read %s: %w", goSumName, err)
	}

	lines := 0

	for index, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		lineNumber := index + 1

		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "//") {
			continue
		}

		lines++

		if err := parseGoSumLine(line); err != nil {
			return fmt.Errorf("%s line %d: %w", goSumName, lineNumber, err)
		}
	}

	if lines == 0 {
		return fmt.Errorf("%s has no entries", goSumName)
	}

	return nil
}

// parseGoSumLine validates one non-comment go.sum entry.
func parseGoSumLine(line string) error {
	fields := strings.Fields(line)
	if len(fields) != 3 {
		return fmt.Errorf("want 3 fields (<module> <version> <hash>), found %d: %q", len(fields), line)
	}

	module, version, hash := fields[0], fields[1], fields[2]

	if module == "" {
		return errors.New("module path is empty")
	}

	baseVersion := strings.TrimSuffix(version, "/go.mod")
	if !strings.HasPrefix(baseVersion, "v") || len(baseVersion) == 1 {
		return fmt.Errorf("version %q does not start with a v-prefixed revision", version)
	}

	algorithm, digest, ok := strings.Cut(hash, ":")
	if !ok {
		return fmt.Errorf("hash %q has no algorithm prefix", hash)
	}

	if algorithm != "h1" {
		return fmt.Errorf("hash %q uses unsupported algorithm %q, want h1", hash, algorithm)
	}

	decoded, err := base64.StdEncoding.DecodeString(digest)
	if err != nil {
		return fmt.Errorf("hash %q is not valid base64: %w", hash, err)
	}

	if len(decoded) != 32 {
		return fmt.Errorf("hash %q decodes to %d bytes, want a 32-byte SHA-256 digest", hash, len(decoded))
	}

	return nil
}

// CheckDocumentedMakeTargets verifies that every make target named in the
// repository documentation exists in the Makefile. It does not require an
// exact target set: adding a target that no document mentions is fine, while a
// document that tells a reader to run `make missing` is not.
//
// Only `make <target>` inside a fenced code block or an inline code span is
// treated as documentation of a target. English prose uses the same three
// words ("this would make the event invalid"), and treating that as a target
// reference would make the check fail for writing style rather than for a
// stale document.
func CheckDocumentedMakeTargets(root string) error {
	data, err := os.ReadFile(filepath.Join(root, makefileName))
	if err != nil {
		return fmt.Errorf("read %s: %w", makefileName, err)
	}

	targets := makefileTargets(string(data))
	if len(targets) == 0 {
		return fmt.Errorf("%s defines no targets", makefileName)
	}

	documents, err := documentationFiles(root)
	if err != nil {
		return err
	}

	type missingTarget struct {
		target string
		where  string
	}

	seen := map[string]bool{}
	missing := make([]missingTarget, 0)

	for _, document := range documents {
		for _, mention := range documentedMakeTargets(document.text) {
			if _, ok := targets[mention.target]; ok {
				continue
			}

			if seen[mention.target] {
				continue
			}

			seen[mention.target] = true
			missing = append(missing, missingTarget{
				target: mention.target,
				where:  fmt.Sprintf("%s:%d", document.name, mention.line),
			})
		}
	}

	if len(missing) > 0 {
		sort.Slice(missing, func(i, j int) bool { return missing[i].target < missing[j].target })

		detail := make([]string, 0, len(missing))
		for _, item := range missing {
			detail = append(detail, fmt.Sprintf("%s is documented at %s but is not a target", item.target, item.where))
		}

		return fmt.Errorf("%s:\n  %s", makefileName, strings.Join(detail, "\n  "))
	}

	return nil
}

// makefileTargets collects the literal target words the Makefile defines.
// Recipe lines (which start with a tab), comments, variable assignments
// (`NAME := value`, `NAME ?= value`) and rule prerequisites are ignored.
func makefileTargets(text string) map[string]struct{} {
	targets := map[string]struct{}{}

	for _, raw := range strings.Split(text, "\n") {
		if strings.HasPrefix(raw, "\t") {
			continue
		}

		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		colon := strings.Index(line, ":")
		if colon < 0 {
			continue
		}

		// `NAME := value` is an assignment, not a rule. `NAME = a:b` is an
		// assignment whose value happens to contain a colon.
		if colon+1 < len(line) && line[colon+1] == '=' {
			continue
		}

		head := line[:colon]
		if strings.ContainsAny(head, "=?") {
			continue
		}

		for _, target := range strings.Fields(head) {
			targets[target] = struct{}{}
		}
	}

	return targets
}

// document is one documentation file and its text.
type document struct {
	name string
	text string
}

// documentationFiles returns README.md and every regular file under docs/.
func documentationFiles(root string) ([]document, error) {
	readme, err := os.ReadFile(filepath.Join(root, "README.md"))
	if err != nil {
		return nil, fmt.Errorf("read README.md: %w", err)
	}

	documents := []document{{name: "README.md", text: string(readme)}}

	docsDir := filepath.Join(root, "docs")
	info, err := os.Stat(docsDir)
	if err != nil {
		return nil, fmt.Errorf("stat docs/: %w", err)
	}

	if !info.IsDir() {
		return nil, errors.New("docs is not a directory")
	}

	err = filepath.WalkDir(docsDir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}

		if entry.IsDir() {
			return nil
		}

		text, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}

		relative, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}

		documents = append(documents, document{name: filepath.ToSlash(relative), text: string(text)})

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk docs/: %w", err)
	}

	return documents, nil
}

// documentedMakeRE matches a `make <target>` invocation. A target here is a
// name that starts with an alphanumeric character, which excludes flags such
// as `make -j4` and `make VAR=value`.
var documentedMakeRE = regexp.MustCompile(`\bmake\s+([A-Za-z0-9][A-Za-z0-9_.-]*)`)

// makeTargetMention is one `make <target>` occurrence and the line it is on.
type makeTargetMention struct {
	target string
	line   int
}

// documentedMakeTargets extracts `make <target>` mentions from Markdown,
// considering only fenced code blocks and inline code spans. See
// CheckDocumentedMakeTargets for why prose is excluded.
func documentedMakeTargets(text string) []makeTargetMention {
	var mentions []makeTargetMention

	for _, segment := range codeSegments(text) {
		for _, match := range documentedMakeRE.FindAllStringSubmatch(segment.text, -1) {
			mentions = append(mentions, makeTargetMention{target: match[1], line: segment.line})
		}
	}

	return mentions
}

// codeSegment is one piece of Markdown that is marked up as code.
type codeSegment struct {
	line int
	text string
}

// codeSegments returns fenced code block lines and inline code spans in the
// order they appear. Fence markers themselves are excluded.
func codeSegments(text string) []codeSegment {
	var segments []codeSegment

	inFence := false
	fenceMarker := ""

	for index, raw := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(raw)

		if marker := fenceMarkerOf(trimmed); marker != "" {
			switch {
			case !inFence:
				inFence = true
				fenceMarker = marker
				continue
			case marker == fenceMarker:
				inFence = false
				fenceMarker = ""
				continue
			}
		}

		if inFence {
			segments = append(segments, codeSegment{line: index + 1, text: raw})
			continue
		}

		for _, span := range inlineCodeSpans(raw) {
			segments = append(segments, codeSegment{line: index + 1, text: span})
		}
	}

	return segments
}

// fenceMarkerOf reports the backtick or tilde run that opens or closes a
// fenced code block, or "" when the line is not a fence marker.
func fenceMarkerOf(trimmed string) string {
	for _, marker := range []byte{'`', '~'} {
		count := 0
		for count < len(trimmed) && trimmed[count] == marker {
			count++
		}

		if count >= 3 {
			return trimmed[:count]
		}
	}

	return ""
}

// inlineCodeSpans returns the contents of the backtick-delimited spans on one
// line. An unmatched backtick is ignored rather than treated as the start of
// an unterminated span.
func inlineCodeSpans(line string) []string {
	var spans []string

	for index := 0; index < len(line); {
		if line[index] != '`' {
			index++
			continue
		}

		start := index
		for index < len(line) && line[index] == '`' {
			index++
		}

		delimiter := line[start:index]
		end := strings.Index(line[index:], delimiter)
		if end < 0 {
			break
		}

		spans = append(spans, line[index:index+end])
		index += end + len(delimiter)
	}

	return spans
}

// yamlKind is the kind of node the minimal YAML reader produced.
type yamlKind int

const (
	yamlScalar yamlKind = iota
	yamlMapping
	yamlSequence
)

// yamlNode is one node of a parsed YAML document. Only the subset the workflow
// needs is represented: block mappings, block sequences and scalars, including
// literal and folded block scalars.
type yamlNode struct {
	kind   yamlKind
	line   int
	scalar string
	keys   []string
	fields map[string]*yamlNode
	items  []*yamlNode
}

// field returns a mapping entry.
func (n *yamlNode) field(name string) (*yamlNode, bool) {
	if n == nil || n.kind != yamlMapping {
		return nil, false
	}

	value, ok := n.fields[name]

	return value, ok
}

// walkMappings calls visit for this node if it is a mapping, then descends
// into every child mapping and sequence.
func (n *yamlNode) walkMappings(visit func(*yamlNode)) {
	if n == nil {
		return
	}

	switch n.kind {
	case yamlMapping:
		visit(n)

		for _, key := range n.keys {
			n.fields[key].walkMappings(visit)
		}

	case yamlSequence:
		for _, item := range n.items {
			item.walkMappings(visit)
		}
	}
}

// yamlLine is one physical line with its indentation and comment-stripped
// text. raw is kept for block scalar content, where a leading # is content
// rather than a comment.
type yamlLine struct {
	number int
	indent int
	text   string
	raw    string
	blank  bool
}

// yamlParser is a cursor over yamlLine values.
type yamlParser struct {
	lines []yamlLine
	pos   int
}

// splitYAMLLines normalises line endings, rejects tab indentation (which YAML
// forbids) and strips comments outside quoted scalars.
func splitYAMLLines(text string) ([]yamlLine, error) {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")

	rawLines := strings.Split(text, "\n")
	lines := make([]yamlLine, 0, len(rawLines))

	for index, raw := range rawLines {
		indent := 0
		for indent < len(raw) && raw[indent] == ' ' {
			indent++
		}

		if indent < len(raw) && raw[indent] == '\t' {
			return nil, fmt.Errorf("line %d: tab used for indentation; YAML requires spaces", index+1)
		}

		content, err := stripYAMLComment(raw[indent:])
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", index+1, err)
		}

		content = strings.TrimRight(content, " \t")
		lines = append(lines, yamlLine{
			number: index + 1,
			indent: indent,
			text:   content,
			raw:    raw,
			blank:  content == "",
		})
	}

	return lines, nil
}

// stripYAMLComment removes a trailing comment. A # starts a comment only at
// the start of the line or after whitespace, and never inside a quoted scalar.
func stripYAMLComment(text string) (string, error) {
	var quote byte

	for index := 0; index < len(text); index++ {
		switch {
		case quote == 0:
			switch text[index] {
			case '\'', '"':
				quote = text[index]
			case '#':
				if index == 0 || text[index-1] == ' ' || text[index-1] == '\t' {
					return text[:index], nil
				}
			}

		case quote == '"':
			if text[index] == '\\' {
				index++
				continue
			}

			if text[index] == '"' {
				quote = 0
			}

		case quote == '\'':
			if text[index] == '\'' {
				if index+1 < len(text) && text[index+1] == '\'' {
					index++
					continue
				}

				quote = 0
			}
		}
	}

	if quote != 0 {
		return "", errors.New("unterminated quoted scalar")
	}

	return text, nil
}

// parseYAML parses the block-style YAML subset used by the CI workflow. It is
// deliberately small: no anchors, aliases, tags or flow collections. Anything
// outside the subset is an error rather than being guessed at, because the
// point of the parse is to notice a workflow that stopped being well formed.
//
// go.mod has no YAML dependency: yaml.v3 appears in the module graph (and in
// go.sum) through this module's dependencies, but it is not in a require
// directive, so importing it would mean editing go.mod -- the artefact this
// package exists to check. This reader is therefore hand-written for the
// subset the workflow uses.
func parseYAML(text string) (*yamlNode, error) {
	lines, err := splitYAMLLines(text)
	if err != nil {
		return nil, err
	}

	parser := &yamlParser{lines: lines}
	parser.skipIgnorable()

	if parser.pos >= len(parser.lines) {
		return nil, errors.New("document has no content")
	}

	node, err := parser.parseBlock(parser.lines[parser.pos].indent)
	if err != nil {
		return nil, err
	}

	parser.skipIgnorable()

	if parser.pos < len(parser.lines) {
		line := parser.lines[parser.pos]

		return nil, fmt.Errorf(
			"line %d: unexpected content %q at indentation %d after the last block",
			line.number, line.text, line.indent,
		)
	}

	return node, nil
}

// skipIgnorable advances past blank and comment-only lines.
func (p *yamlParser) skipIgnorable() {
	for p.pos < len(p.lines) && p.lines[p.pos].blank {
		p.pos++
	}
}

// parseBlock parses a mapping or sequence that starts at indent.
func (p *yamlParser) parseBlock(indent int) (*yamlNode, error) {
	if p.pos >= len(p.lines) {
		return nil, errors.New("unexpected end of document, expected a block")
	}

	line := p.lines[p.pos]
	if line.indent != indent {
		return nil, fmt.Errorf("line %d: expected indentation %d, found %d", line.number, indent, line.indent)
	}

	if isSequenceItem(line.text) {
		return p.parseSequence(indent)
	}

	return p.parseMapping(indent)
}

// isSequenceItem reports whether text begins a block sequence entry.
func isSequenceItem(text string) bool {
	return text == "-" || strings.HasPrefix(text, "- ")
}

// parseMapping parses consecutive `key: value` entries at indent.
func (p *yamlParser) parseMapping(indent int) (*yamlNode, error) {
	node := &yamlNode{
		kind:   yamlMapping,
		line:   p.lines[p.pos].number,
		fields: map[string]*yamlNode{},
	}

	for {
		p.skipIgnorable()

		if p.pos >= len(p.lines) {
			return node, nil
		}

		line := p.lines[p.pos]

		if line.indent < indent {
			return node, nil
		}

		if line.indent > indent {
			return nil, fmt.Errorf(
				"line %d: unexpected indentation %d inside the mapping that starts at indentation %d",
				line.number, line.indent, indent,
			)
		}

		if isSequenceItem(line.text) {
			return nil, fmt.Errorf("line %d: sequence item at mapping indentation %d", line.number, indent)
		}

		key, rest, err := splitMappingEntry(line.text, line.number)
		if err != nil {
			return nil, err
		}

		if _, duplicate := node.fields[key]; duplicate {
			return nil, fmt.Errorf("line %d: duplicate mapping key %q", line.number, key)
		}

		p.pos++

		value, valueErr := p.parseMappingValue(indent, rest, line.number)
		if valueErr != nil {
			return nil, valueErr
		}

		node.keys = append(node.keys, key)
		node.fields[key] = value
	}
}

// parseMappingValue parses the value after `key:`, which may be empty (with a
// nested block on the following lines), a block scalar indicator, or a scalar
// on the same line.
func (p *yamlParser) parseMappingValue(parentIndent int, rest string, lineNumber int) (*yamlNode, error) {
	if rest == "" {
		p.skipIgnorable()

		if p.pos < len(p.lines) && p.lines[p.pos].indent > parentIndent {
			return p.parseBlock(p.lines[p.pos].indent)
		}

		return &yamlNode{kind: yamlScalar, line: lineNumber}, nil
	}

	if style, chomp, ok := blockScalarIndicator(rest); ok {
		return p.parseBlockScalar(parentIndent, style, chomp, lineNumber)
	}

	value, err := parseInlineScalar(rest)
	if err != nil {
		return nil, fmt.Errorf("line %d: %w", lineNumber, err)
	}

	return &yamlNode{kind: yamlScalar, line: lineNumber, scalar: value}, nil
}

// blockScalarIndicator recognises the |, >, |- , >- , |+ and >+ forms.
func blockScalarIndicator(rest string) (style, chomp byte, ok bool) {
	if rest == "" || (rest[0] != '|' && rest[0] != '>') {
		return 0, 0, false
	}

	style = rest[0]

	for index := 1; index < len(rest); index++ {
		switch rest[index] {
		case '-', '+':
			if chomp != 0 {
				return 0, 0, false
			}

			chomp = rest[index]

		case '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
			// An explicit indentation indicator is accepted and ignored; the
			// content indentation is measured from the block instead.

		default:
			return 0, 0, false
		}
	}

	return style, chomp, true
}

// parseBlockScalar consumes a literal or folded block scalar. Blank lines are
// part of the block; content indentation is the minimum indentation of the
// non-blank content lines.
func (p *yamlParser) parseBlockScalar(parentIndent int, style, chomp byte, lineNumber int) (*yamlNode, error) {
	start := p.pos
	contentIndent := -1
	end := start

	for end < len(p.lines) {
		line := p.lines[end]

		if line.blank {
			end++
			continue
		}

		if line.indent <= parentIndent {
			break
		}

		if contentIndent < 0 || line.indent < contentIndent {
			contentIndent = line.indent
		}

		end++
	}

	// Trailing blank lines are not content, and leaving them for
	// skipIgnorable keeps the surrounding block's cursor honest.
	for end > start && p.lines[end-1].blank {
		end--
	}

	content := make([]string, 0, end-start)

	for index := start; index < end; index++ {
		line := p.lines[index]

		if line.blank {
			content = append(content, "")
			continue
		}

		dedented := ""
		if contentIndent >= 0 && len(line.raw) >= contentIndent {
			dedented = line.raw[contentIndent:]
		}

		content = append(content, strings.TrimRight(dedented, " \t"))
	}

	var value string

	if style == '>' {
		var builder strings.Builder

		for index, line := range content {
			if index > 0 {
				if line == "" || content[index-1] == "" {
					builder.WriteByte('\n')
				} else {
					builder.WriteByte(' ')
				}
			}

			builder.WriteString(line)
		}

		value = builder.String()
	} else {
		value = strings.Join(content, "\n")
	}

	if chomp != '-' && value != "" {
		value += "\n"
	}

	p.pos = end

	return &yamlNode{kind: yamlScalar, line: lineNumber, scalar: value}, nil
}

// parseSequence parses consecutive `- item` entries at indent.
func (p *yamlParser) parseSequence(indent int) (*yamlNode, error) {
	node := &yamlNode{kind: yamlSequence, line: p.lines[p.pos].number}

	for {
		p.skipIgnorable()

		if p.pos >= len(p.lines) {
			return node, nil
		}

		line := p.lines[p.pos]

		if line.indent < indent {
			return node, nil
		}

		if line.indent > indent {
			return nil, fmt.Errorf(
				"line %d: unexpected indentation %d inside the sequence that starts at indentation %d",
				line.number, line.indent, indent,
			)
		}

		if !isSequenceItem(line.text) {
			return nil, fmt.Errorf("line %d: expected a sequence item ('- '), found %q", line.number, line.text)
		}

		body := line.text[1:]

		spaces := 0
		for spaces < len(body) && body[spaces] == ' ' {
			spaces++
		}

		inline := strings.TrimSpace(body[spaces:])
		p.pos++

		var child *yamlNode

		var err error

		switch {
		case inline == "":
			p.skipIgnorable()

			if p.pos < len(p.lines) && p.lines[p.pos].indent > indent {
				child, err = p.parseBlock(p.lines[p.pos].indent)
			} else {
				child = &yamlNode{kind: yamlScalar, line: line.number}
			}

		case isInlineMappingEntry(inline):
			childIndent := indent + 1 + spaces

			// The text after `- ` is the first entry of a mapping. Insert it
			// as a synthetic line at the column the text starts in, so the
			// mapping parser sees one consistent indentation for the item.
			synthetic := yamlLine{number: line.number, indent: childIndent, text: inline, raw: inline}
			p.lines = append(p.lines[:p.pos], append([]yamlLine{synthetic}, p.lines[p.pos:]...)...)
			child, err = p.parseMapping(childIndent)

		default:
			var value string

			value, err = parseInlineScalar(inline)
			if err == nil {
				child = &yamlNode{kind: yamlScalar, line: line.number, scalar: value}
			}
		}

		if err != nil {
			return nil, err
		}

		node.items = append(node.items, child)
	}
}

// splitMappingEntry splits `key: value` at the first colon that is followed by
// whitespace or the end of the line and is not inside a quoted key.
func splitMappingEntry(text string, lineNumber int) (key, rest string, err error) {
	colon := findMappingColon(text)
	if colon < 0 {
		return "", "", fmt.Errorf("line %d: expected 'key: value', found %q", lineNumber, text)
	}

	key = strings.TrimSpace(text[:colon])
	if key == "" {
		return "", "", fmt.Errorf("line %d: empty mapping key", lineNumber)
	}

	if strings.HasPrefix(key, `"`) || strings.HasPrefix(key, `'`) {
		unquoted, unquoteErr := unquoteYAMLScalar(key)
		if unquoteErr != nil {
			return "", "", fmt.Errorf("line %d: %w", lineNumber, unquoteErr)
		}

		key = unquoted
	}

	return key, strings.TrimSpace(text[colon+1:]), nil
}

// isInlineMappingEntry reports whether a sequence item's inline text starts a
// mapping rather than being a plain scalar.
func isInlineMappingEntry(text string) bool {
	return findMappingColon(text) >= 0
}

// findMappingColon returns the index of the colon that separates a mapping key
// from its value, or -1 when text is not a mapping entry.
func findMappingColon(text string) int {
	var quote byte

	for index := 0; index < len(text); index++ {
		switch {
		case quote == 0:
			switch text[index] {
			case '\'', '"':
				quote = text[index]
			case ':':
				if index+1 == len(text) || text[index+1] == ' ' || text[index+1] == '\t' {
					return index
				}
			}

		case quote == '\'':
			if text[index] == '\'' {
				if index+1 < len(text) && text[index+1] == '\'' {
					index++
					continue
				}

				quote = 0
			}

		case quote == '"':
			if text[index] == '\\' {
				index++
				continue
			}

			if text[index] == '"' {
				quote = 0
			}
		}
	}

	return -1
}

// parseInlineScalar unquotes a scalar written on the same line as its key.
func parseInlineScalar(text string) (string, error) {
	if text == "" {
		return "", nil
	}

	if text[0] != '"' && text[0] != '\'' {
		return text, nil
	}

	return unquoteYAMLScalar(text)
}

// unquoteYAMLScalar removes single or double quotes from a complete scalar.
func unquoteYAMLScalar(text string) (string, error) {
	if len(text) < 2 {
		return "", errors.New("quoted scalar is not terminated")
	}

	switch text[0] {
	case '\'':
		if text[len(text)-1] != '\'' {
			return "", errors.New("single-quoted scalar is not terminated")
		}

		return strings.ReplaceAll(text[1:len(text)-1], "''", "'"), nil

	case '"':
		value, err := strconv.Unquote(text)
		if err != nil {
			return "", fmt.Errorf("invalid double-quoted scalar %q: %w", text, err)
		}

		return value, nil
	}

	return text, nil
}

// loadWorkflow reads and parses the CI workflow. It returns the document tree
// and the source text, so checks that care about structure and checks that care
// about the commands can share one read without sharing one interpretation.
func loadWorkflow(root string) (*yamlNode, string, error) {
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(ciWorkflow)))
	if err != nil {
		return nil, "", fmt.Errorf("read %s: %w", ciWorkflow, err)
	}

	document, err := parseYAML(string(data))
	if err != nil {
		return nil, "", fmt.Errorf("%s: %w", ciWorkflow, err)
	}

	return document, string(data), nil
}

// CheckWorkflowParses verifies that the CI workflow is valid YAML in the block
// style it uses, and that it declares at least one job under a test key.
func CheckWorkflowParses(root string) error {
	document, _, err := loadWorkflow(root)
	if err != nil {
		return err
	}

	if document.kind != yamlMapping {
		return fmt.Errorf("%s: the top level is not a mapping", ciWorkflow)
	}

	jobs, ok := document.field("jobs")
	if !ok || jobs.kind != yamlMapping || len(jobs.keys) == 0 {
		return fmt.Errorf("%s declares no jobs", ciWorkflow)
	}

	if _, ok := jobs.field("test"); !ok {
		return fmt.Errorf("%s has no test job; jobs are %v", ciWorkflow, jobs.keys)
	}

	return nil
}

// CheckCIRunnerPins verifies that every runs-on value names a pinned runner
// image: a distro with a version (ubuntu-24.04) or the explicit latest alias
// (ubuntu-latest), rather than a bare distro name whose image can change
// underneath CI without a commit.
func CheckCIRunnerPins(root string) error {
	document, _, err := loadWorkflow(root)
	if err != nil {
		return err
	}

	images := runnerImages(document)
	if len(images) == 0 {
		return fmt.Errorf("%s declares no runs-on value", ciWorkflow)
	}

	var unpinned []string

	for _, image := range images {
		if !isPinnedRunner(image) {
			unpinned = append(unpinned, image)
		}
	}

	if len(unpinned) > 0 {
		return fmt.Errorf(
			"%s has unpinned runner image(s) %q; want a versioned image such as ubuntu-24.04 or ubuntu-latest (found %v)",
			ciWorkflow, unpinned, images,
		)
	}

	return nil
}

// runnerImages collects every runs-on value in the workflow, including one
// inside a job matrix or a nested reusable workflow.
func runnerImages(document *yamlNode) []string {
	var images []string

	document.walkMappings(func(mapping *yamlNode) {
		value, ok := mapping.field("runs-on")
		if !ok {
			return
		}

		switch value.kind {
		case yamlScalar:
			images = append(images, value.scalar)

		case yamlSequence:
			for _, item := range value.items {
				if item.kind == yamlScalar {
					images = append(images, item.scalar)
				}
			}
		}
	})

	return images
}

// isPinnedRunner reports whether a runner image carries a version. A bare
// "ubuntu" or "self-hosted" is not pinned; "ubuntu-24.04", "macos-14" and
// "ubuntu-latest" are.
func isPinnedRunner(image string) bool {
	image = strings.TrimSpace(image)
	if image == "" || strings.Contains(image, "${{") {
		return false
	}

	parts := strings.Split(image, "-")
	if len(parts) < 2 {
		return false
	}

	if parts[len(parts)-1] == "latest" {
		return true
	}

	for _, part := range parts[1:] {
		if part != "" && part[0] >= '0' && part[0] <= '9' {
			return true
		}
	}

	return false
}

// coreNodeMarkers and blakeNodeMarkers are the names a node download or
// installation line may use. The BLAKE2b node in this repository is a Bitcoin
// Knots build carrying the BLAKE2b proof-of-work hardfork (see
// scripts/test-nodes and tests/node_config.py), so both the literal
// bitcoin-blake2b name and the repository's Knots vocabulary are accepted; a
// workflow that provisions neither is the failure this check exists for.
var (
	coreNodeMarkers  = []string{"bitcoincore.org", "bitcoin-core-", "bitcoin core"}
	blakeNodeMarkers = []string{"bitcoin-blake2b", "bitcoin-blake-", "bitcoinknots.org", "bitcoin-knots"}
)

// CheckCIProvisionsBothNodeBinaries verifies that the workflow provisions two
// distinct node builds: a Bitcoin Core build and a BLAKE2b-chain build.
//
// It also checks that the two downloads come from distinct hosts, that the
// downloads are checksum-verified, and, when the workflow pins versions and
// checksums in variables, that the pins are distinct and actually used. Those
// are the properties the reference packaging suite asserts, and they are what
// make "two binaries" mean two implementations rather than one binary used
// twice.
func CheckCIProvisionsBothNodeBinaries(root string) error {
	_, text, err := loadWorkflow(root)
	if err != nil {
		return err
	}

	active := strings.Join(activeLines(text), "\n")

	core := firstMarker(active, coreNodeMarkers)
	if core == "" {
		return fmt.Errorf(
			"%s never downloads or names a Bitcoin Core build; looked for %v",
			ciWorkflow, coreNodeMarkers,
		)
	}

	blake := firstMarker(active, blakeNodeMarkers)
	if blake == "" {
		return fmt.Errorf(
			"%s never downloads or names a BLAKE2b node build; looked for %v",
			ciWorkflow, blakeNodeMarkers,
		)
	}

	hosts := nodeDownloadHosts(active)
	if len(hosts) < 2 {
		return fmt.Errorf(
			"%s must download the two node builds from distinct hosts, so one binary cannot stand in for both chains; found %v",
			ciWorkflow, hosts,
		)
	}

	if !strings.Contains(active, "sha256sum -c -") {
		return fmt.Errorf("%s downloads node binaries but never verifies a checksum (sha256sum -c -)", ciWorkflow)
	}

	pins := workflowPins(text)

	versions := pinsWithSuffix(pins, "_VERSION")
	hashes := pinsWithSuffix(pins, "_SHA256")

	if len(versions) > 0 {
		if len(versions) < 2 {
			return fmt.Errorf("%s pins only one node version: %v", ciWorkflow, versions)
		}

		if distinctValues(versions) < 2 {
			return fmt.Errorf("%s pins both node builds to the same version: %v", ciWorkflow, versions)
		}

		for name := range versions {
			if !strings.Contains(text, "${"+name+"}") {
				return fmt.Errorf("%s pins %s but no download URL uses ${%s}", ciWorkflow, name, name)
			}
		}
	}

	if len(hashes) > 0 {
		if len(hashes) < 2 {
			return fmt.Errorf("%s pins only one node checksum: %v", ciWorkflow, hashes)
		}

		if distinctValues(hashes) < 2 {
			return fmt.Errorf("%s pins both node binaries to the same checksum, so one is not verified: %v", ciWorkflow, hashes)
		}

		for name, value := range hashes {
			if !sha256ValueRE.MatchString(value) {
				return fmt.Errorf("%s pins %s to %q, which is not a SHA-256 hex digest", ciWorkflow, name, value)
			}

			if !strings.Contains(text, "${"+name+"}") {
				return fmt.Errorf("%s pins %s but the checksum line does not use ${%s}", ciWorkflow, name, name)
			}
		}
	}

	return nil
}

// sha256ValueRE matches a SHA-256 digest as a workflow env value writes it.
var sha256ValueRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

// workflowPinRE matches a quoted `NAME: "value"` entry, which is how the
// workflow declares its version and checksum pins.
var workflowPinRE = regexp.MustCompile(`(?m)^[ \t]*([A-Z][A-Z0-9_]*):[ \t]*"([^"]*)"[ \t]*$`)

// workflowPins returns the quoted uppercase KEY: "value" entries in text.
func workflowPins(text string) map[string]string {
	pins := map[string]string{}

	for _, match := range workflowPinRE.FindAllStringSubmatch(text, -1) {
		pins[match[1]] = match[2]
	}

	return pins
}

// pinsWithSuffix filters a pin map by variable-name suffix.
func pinsWithSuffix(pins map[string]string, suffix string) map[string]string {
	filtered := map[string]string{}

	for name, value := range pins {
		if strings.HasSuffix(name, suffix) {
			filtered[name] = value
		}
	}

	return filtered
}

// distinctValues counts the distinct values in a pin map.
func distinctValues(values map[string]string) int {
	seen := map[string]struct{}{}

	for _, value := range values {
		seen[value] = struct{}{}
	}

	return len(seen)
}

// nodeDownloadHosts returns the distinct hosts in the active lines from which
// something with bitcoin in its URL is downloaded.
func nodeDownloadHosts(text string) []string {
	var hosts []string

	seen := map[string]struct{}{}

	for _, raw := range httpURLRE.FindAllString(text, -1) {
		if !strings.Contains(strings.ToLower(raw), "bitcoin") {
			continue
		}

		parsed, err := url.Parse(raw)
		if err != nil {
			continue
		}

		host := parsed.Hostname()
		if host == "" {
			continue
		}

		if _, duplicate := seen[host]; duplicate {
			continue
		}

		seen[host] = struct{}{}
		hosts = append(hosts, host)
	}

	sort.Strings(hosts)

	return hosts
}

// httpURLRE matches an http(s) URL in a workflow command line.
var httpURLRE = regexp.MustCompile("https?://[^\\s\"'`)]+")

// firstMarker returns the first marker present in text, or "".
func firstMarker(text string, markers []string) string {
	lower := strings.ToLower(text)

	for _, marker := range markers {
		if strings.Contains(lower, marker) {
			return marker
		}
	}

	return ""
}

// CheckCIRunsUnitAndWasmGates verifies that the workflow actually runs both
// gates the project documents: the unit suite gate (scripts/test.sh
// --unit-only) and the wasm build gate (make wasm). Mentioning either in a
// comment does not count.
func CheckCIRunsUnitAndWasmGates(root string) error {
	_, text, err := loadWorkflow(root)
	if err != nil {
		return err
	}

	var (
		unitGate bool
		wasmGate bool
	)

	for _, line := range activeLines(text) {
		if strings.Contains(line, "scripts/test.sh") && strings.Contains(line, "--unit-only") {
			unitGate = true
		}

		if makeWasmRE.MatchString(line) {
			wasmGate = true
		}
	}

	var missing []string

	if !unitGate {
		missing = append(missing, "the unit gate (scripts/test.sh --unit-only)")
	}

	if !wasmGate {
		missing = append(missing, "the wasm gate (make wasm)")
	}

	if len(missing) > 0 {
		return fmt.Errorf("%s does not run %s", ciWorkflow, strings.Join(missing, " or "))
	}

	return nil
}

// makeWasmRE matches a `make wasm` invocation but not `make wasmtest`.
var makeWasmRE = regexp.MustCompile(`\bmake\s+wasm\b`)

// publicRelayHosts are well-known public Nostr relays. A merge-blocking job
// that names one of them makes a third party's availability part of the gate.
var publicRelayHosts = []string{
	"nos.lol",
	"relay.damus.io",
	"damus.io",
	"nostr.wine",
	"relay.nostr.band",
	"purplepag.es",
	"nostr.mom",
	"relay.snort.social",
	"eden.nostr.land",
}

// relayURLRE matches a ws:// or wss:// URL.
var relayURLRE = regexp.MustCompile("wss?://[^\\s\"'`)]+")

// CheckCINoPublicRelayGate verifies that no job that must pass depends on a
// public Nostr relay: the public-relay suites are not invoked, no known public
// relay hostname appears in an active line, and no non-local ws:// or wss://
// URL is used. Comment lines are ignored, so documenting the flag is fine;
// invoking it is not.
func CheckCINoPublicRelayGate(root string) error {
	_, text, err := loadWorkflow(root)
	if err != nil {
		return err
	}

	var problems []string

	for _, line := range activeLines(text) {
		if strings.Contains(line, "--with-nostr") {
			problems = append(problems, "invokes the public-relay suites: "+line)
		}

		lower := strings.ToLower(line)

		for _, host := range publicRelayHosts {
			if strings.Contains(lower, host) {
				problems = append(problems, fmt.Sprintf("names the public relay %q: %s", host, line))
			}
		}

		if external := externalRelayURL(line); external != "" {
			problems = append(problems, "uses an external relay URL: "+external)
		}
	}

	if len(problems) > 0 {
		return fmt.Errorf(
			"%s makes a merge-blocking job depend on a public relay:\n  %s",
			ciWorkflow, strings.Join(problems, "\n  "),
		)
	}

	return nil
}

// externalRelayURL returns the first ws:// or wss:// URL in line whose host is
// not loopback.
func externalRelayURL(line string) string {
	for _, raw := range relayURLRE.FindAllString(line, -1) {
		parsed, err := url.Parse(raw)
		if err != nil {
			continue
		}

		switch parsed.Hostname() {
		case "", "localhost", "127.0.0.1", "::1", "0.0.0.0":
			continue
		}

		return raw
	}

	return ""
}

// activeLines returns the trimmed, non-empty, non-comment lines of text. It is
// used for command checks so that a flag or hostname mentioned in a comment is
// not mistaken for an invocation.
func activeLines(text string) []string {
	var lines []string

	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		lines = append(lines, line)
	}

	return lines
}

// CheckLicense verifies that LICENSE is present and non-empty.
func CheckLicense(root string) error {
	data, err := os.ReadFile(filepath.Join(root, licenseName))
	if err != nil {
		return fmt.Errorf("read %s: %w", licenseName, err)
	}

	if strings.TrimSpace(string(data)) == "" {
		return fmt.Errorf("%s is empty", licenseName)
	}

	return nil
}

// CheckPackageLayout verifies that every directory under pkg/ and cmd/ that
// contains Go files belongs to this module: it has no own go.mod. A nested
// module silently drops its packages from `go build ./...` and from the
// module's dependency graph, which is exactly the class of mistake this check
// is for.
//
// It does not run `go build ./...`: compiling the tree is the toolchain's gate,
// and this check is about the layout the toolchain is given.
func CheckPackageLayout(root string) error {
	var problems []string

	packages := 0

	for _, base := range []string{"pkg", "cmd"} {
		basePath := filepath.Join(root, base)

		info, err := os.Stat(basePath)
		if err != nil {
			return fmt.Errorf("stat %s: %w", base, err)
		}

		if !info.IsDir() {
			return fmt.Errorf("%s is not a directory", base)
		}

		walkErr := filepath.WalkDir(basePath, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}

			if !entry.IsDir() || path == basePath {
				return nil
			}

			entries, readErr := os.ReadDir(path)
			if readErr != nil {
				return readErr
			}

			hasGo := false

			for _, item := range entries {
				if !item.IsDir() && strings.HasSuffix(item.Name(), ".go") {
					hasGo = true
					break
				}
			}

			if !hasGo {
				return nil
			}

			packages++

			nested := filepath.Join(path, goModName)

			if _, statErr := os.Stat(nested); statErr == nil {
				relative, relErr := filepath.Rel(root, path)
				if relErr != nil {
					return relErr
				}

				problems = append(problems, fmt.Sprintf(
					"%s contains Go files but is a separate module (it has its own %s)",
					filepath.ToSlash(relative), goModName,
				))
			} else if !errors.Is(statErr, fs.ErrNotExist) {
				return statErr
			}

			return nil
		})
		if walkErr != nil {
			return fmt.Errorf("walk %s: %w", base, walkErr)
		}
	}

	if packages == 0 {
		return errors.New("no Go package directories found under pkg/ or cmd/")
	}

	if len(problems) > 0 {
		sort.Strings(problems)

		return fmt.Errorf("the module layout is inconsistent:\n  %s", strings.Join(problems, "\n  "))
	}

	return nil
}
