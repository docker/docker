// The parser package implements a parser that transforms a raw byte-stream
// into a low-level Abstract Syntax Tree.
package parser

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"github.com/moby/buildkit/frontend/dockerfile/command"
	"github.com/moby/buildkit/frontend/dockerfile/shell"
	"github.com/pkg/errors"
)

// Node is a structure used to represent a parse tree.
//
// In the node there are three fields, Value, Next, and Children. Value is the
// current token's string value. Next is always the next non-child token, and
// children contains all the children. Here's an example:
//
// (value next (child child-next child-next-next) next-next)
//
// This data structure is frankly pretty lousy for handling complex languages,
// but lucky for us the Dockerfile isn't very complicated. This structure
// works a little more effectively than a "proper" parse tree for our needs.
type Node struct {
	Value       string          // actual content
	Next        *Node           // the next item in the current sexp
	Children    []*Node         // the children of this sexp
	Heredocs    []Heredoc       // extra heredoc content attachments
	Attributes  map[string]bool // special attributes for this node
	Original    string          // original line used before parsing
	Flags       []string        // only top Node should have this set
	StartLine   int             // the line in the original dockerfile where the node begins
	EndLine     int             // the line in the original dockerfile where the node ends
	PrevComment []string
}

// Location return the location of node in source code
func (node *Node) Location() []Range {
	return toRanges(node.StartLine, node.EndLine)
}

// Dump dumps the AST defined by `node` as a list of sexps.
// Returns a string suitable for printing.
func (node *Node) Dump() string {
	var str strings.Builder
	str.WriteString(strings.ToLower(node.Value))

	if len(node.Flags) > 0 {
		fmt.Fprintf(&str, " %q", node.Flags)
	}

	for _, n := range node.Children {
		str.WriteByte('(')
		str.WriteString(n.Dump())
		str.WriteString(")\n")
	}

	for n := node.Next; n != nil; n = n.Next {
		str.WriteByte(' ')
		if len(n.Children) > 0 {
			str.WriteString(n.Dump())
		} else {
			str.WriteString(strconv.Quote(n.Value))
		}
	}

	return strings.TrimSpace(str.String())
}

func (node *Node) lines(start, end int) {
	node.StartLine = start
	node.EndLine = end
}

func (node *Node) canContainHeredoc() bool {
	// check for compound commands, like ONBUILD
	if ok := heredocCompoundDirectives[strings.ToLower(node.Value)]; ok {
		if node.Next != nil && len(node.Next.Children) > 0 {
			node = node.Next.Children[0]
		}
	}

	if ok := heredocDirectives[strings.ToLower(node.Value)]; !ok {
		return false
	}
	if isJSON := node.Attributes["json"]; isJSON {
		return false
	}

	return true
}

// AddChild adds a new child node, and updates line information
func (node *Node) AddChild(child *Node, startLine, endLine int) {
	child.lines(startLine, endLine)
	if node.StartLine < 0 {
		node.StartLine = startLine
	}
	node.EndLine = endLine
	node.Children = append(node.Children, child)
}

type Heredoc struct {
	Name           string
	FileDescriptor uint
	Expand         bool
	Chomp          bool
	Content        string
}

var (
	dispatch      map[string]func(string, *directives) (*Node, map[string]bool, error)
	reWhitespace  = regexp.MustCompile(`[\t\v\f\r ]+`)
	reHeredoc     = regexp.MustCompile(`^(\d*)<<(-?)\s*([^<]*)$`)
	reLeadingTabs = regexp.MustCompile(`(?m)^\t+`)
)

// DefaultEscapeToken is the default escape token
const DefaultEscapeToken = '\\'

var (
	// Directives allowed to contain heredocs
	heredocDirectives = map[string]bool{
		command.Add:  true,
		command.Copy: true,
		command.Run:  true,
	}

	// Directives allowed to contain directives containing heredocs
	heredocCompoundDirectives = map[string]bool{
		command.Onbuild: true,
	}

	// Directives that parse key-value pairs but take a list of names
	// when --unset flag is set
	unsetCommands = map[string]bool{
		command.Env:   true,
		command.Label: true,
	}
)

// directives is the structure used during a build run to hold the state of
// parsing directives.
type directives struct {
	parser                DirectiveParser
	escapeToken           rune           // Current escape token
	lineContinuationRegex *regexp.Regexp // Current line continuation regex
}

// setEscapeToken sets the default token for escaping characters and as line-
// continuation token in a Dockerfile. Only ` (backtick) and \ (backslash) are
// allowed as token.
func (d *directives) setEscapeToken(s string) error {
	if s != "`" && s != `\` {
		return errors.Errorf("invalid escape token '%s' does not match ` or \\", s)
	}
	d.escapeToken = rune(s[0])
	// The escape token is used both to escape characters in a line and as line
	// continuation token. If it's the last non-whitespace token, it is used as
	// line-continuation token, *unless* preceded by an escape-token.
	//
	// The second branch in the regular expression handles line-continuation
	// tokens on their own line, which don't have any character preceding them.
	//
	// Due to Go lacking negative look-ahead matching, this regular expression
	// does not currently handle a line-continuation token preceded by an *escaped*
	// escape-token ("foo \\\").
	d.lineContinuationRegex = regexp.MustCompile(`([^\` + s + `])\` + s + `[ \t]*$|^\` + s + `[ \t]*$`)
	return nil
}

// possibleParserDirective looks for parser directives, eg '# escapeToken=<char>'.
// Parser directives must precede any builder instruction or other comments,
// and cannot be repeated. Returns true if a parser directive was found.
func (d *directives) possibleParserDirective(line []byte) (bool, error) {
	directive, err := d.parser.ParseLine(line)
	if err != nil {
		return false, err
	}
	if directive != nil && directive.Name == keyEscape {
		err := d.setEscapeToken(directive.Value)
		return err == nil, err
	}
	return directive != nil, nil
}

// newDefaultDirectives returns a new directives structure with the default escapeToken token
func newDefaultDirectives() *directives {
	d := &directives{}
	d.setEscapeToken(string(DefaultEscapeToken))
	return d
}

func init() {
	// Dispatch Table. see line_parsers.go for the parse functions.
	// The command is parsed and mapped to the line parser. The line parser
	// receives the arguments but not the command, and returns an AST after
	// reformulating the arguments according to the rules in the parser
	// functions. Errors are propagated up by Parse() and the resulting AST can
	// be incorporated directly into the existing AST as a next.
	dispatch = map[string]func(string, *directives) (*Node, map[string]bool, error){
		command.Add:         parseMaybeJSONToList,
		command.Arg:         parseNameOrNameVal,
		command.Cmd:         parseMaybeJSON,
		command.Copy:        parseMaybeJSONToList,
		command.Entrypoint:  parseMaybeJSON,
		command.Env:         parseEnv,
		command.Expose:      parseStringsWhitespaceDelimited,
		command.From:        parseStringsWhitespaceDelimited,
		command.Healthcheck: parseHealthConfig,
		command.Label:       parseLabel,
		command.Maintainer:  parseString,
		command.Onbuild:     parseSubCommand,
		command.Run:         parseMaybeJSON,
		command.Shell:       parseMaybeJSON,
		command.StopSignal:  parseString,
		command.User:        parseString,
		command.Volume:      parseMaybeJSONToList,
		command.Workdir:     parseString,
	}
}

// newNodeFromLine splits the line into parts, and dispatches to a function
// based on the command and command arguments. A Node is created from the
// result of the dispatch.
func newNodeFromLine(line string, d *directives, comments []string) (*Node, error) {
	cmd, flags, args, err := splitCommand(line, d)
	if err != nil {
		return nil, err
	}

	fn := dispatch[strings.ToLower(cmd)]
	// Ignore invalid Dockerfile instructions
	if fn == nil {
		fn = parseIgnore
	}
	// --unset form takes a list of names instead of key-value pairs
	if unsetCommands[strings.ToLower(cmd)] && hasUnsetFlag(flags) {
		fn = parseNameOrNameVal
	}
	next, attrs, err := fn(args, d)
	if err != nil {
		return nil, err
	}

	return &Node{
		Value:       cmd,
		Original:    line,
		Flags:       flags,
		Next:        next,
		Attributes:  attrs,
		PrevComment: comments,
	}, nil
}

func hasUnsetFlag(flags []string) bool {
	for _, f := range flags {
		name, value, _ := strings.Cut(f, "=")
		if name == "--unset" {
			// invalid values are reported by the flag parser
			return !strings.EqualFold(value, "false")
		}
	}
	return false
}

// Result contains the bundled outputs from parsing a Dockerfile.
type Result struct {
	AST         *Node
	EscapeToken rune
	Warnings    []Warning
}

// Warning contains information to identify and locate a warning generated
// during parsing.
type Warning struct {
	Short    string
	Detail   [][]byte
	URL      string
	Location *Range
}

// PrintWarnings to the writer
func (r *Result) PrintWarnings(out io.Writer) {
	if len(r.Warnings) == 0 {
		return
	}
	for _, w := range r.Warnings {
		fmt.Fprintf(out, "[WARNING]: %s\n", w.Short)
	}
	if len(r.Warnings) > 0 {
		fmt.Fprint(out, "[WARNING]: Empty continuation lines will become errors in a future release.\n")
	}
}

// Parse consumes lines from a provided Reader, parses each line into an AST
// and returns the results of doing so.
func Parse(rwc io.Reader) (*Result, error) {
	d := newDefaultDirectives()
	currentLine := 0
	root := &Node{StartLine: -1}
	scanner := bufio.NewScanner(rwc)
	scanner.Split(scanLines)
	warnings := []Warning{}
	var comments []string
	buf := &bytes.Buffer{}

	var err error
	for scanner.Scan() {
		bytesRead := scanner.Bytes()
		if currentLine == 0 {
			// First line, strip the byte-order-marker if present
			bytesRead = discardBOM(bytesRead)
		}
		if isComment(bytesRead) {
			comment := strings.TrimSpace(string(bytesRead[1:]))
			if comment == "" {
				comments = nil
			} else {
				comments = append(comments, comment)
			}
		}
		var directiveOk bool
		bytesRead, directiveOk, err = processLine(d, bytesRead, true)
		// If the line is a directive, strip it from the comments
		// so it doesn't get added to the AST.
		if directiveOk {
			comments = comments[:len(comments)-1]
		}
		if err != nil {
			return nil, withLocation(err, currentLine, 0)
		}
		currentLine++

		startLine := currentLine
		bytesRead, isEndOfLine := trimContinuationCharacter(bytesRead, d)
		if isEndOfLine && len(bytesRead) == 0 {
			continue
		}
		buf.Reset()
		buf.Write(bytesRead)

		var hasEmptyContinuationLine bool
		for !isEndOfLine && scanner.Scan() {
			bytesRead, _, err := processLine(d, scanner.Bytes(), false)
			if err != nil {
				return nil, withLocation(err, currentLine, 0)
			}
			currentLine++

			if isComment(scanner.Bytes()) {
				// original line was a comment (processLine strips comments)
				continue
			}
			if isEmptyContinuationLine(bytesRead) {
				hasEmptyContinuationLine = true
				continue
			}

			bytesRead, isEndOfLine = trimContinuationCharacter(bytesRead, d)
			buf.Write(bytesRead)
		}

		line := buf.String()

		if hasEmptyContinuationLine {
			warnings = append(warnings, Warning{
				Short:    "Empty continuation line found in: " + line,
				Detail:   [][]byte{[]byte("Empty continuation lines will become errors in a future release")},
				URL:      "https://docs.docker.com/go/dockerfile/rule/no-empty-continuation/",
				Location: &Range{Start: Position{Line: currentLine}, End: Position{Line: currentLine}},
			})
		}

		child, err := newNodeFromLine(line, d, comments)
		if err != nil {
			return nil, withLocation(err, startLine, currentLine)
		}

		if child.canContainHeredoc() && strings.Contains(line, "<<") {
			heredocs, err := heredocsFromLine(line)
			if err != nil {
				return nil, withLocation(err, startLine, currentLine)
			}

			for _, heredoc := range heredocs {
				terminator := []byte(heredoc.Name)
				terminated := false
				var content strings.Builder
				content.WriteString(heredoc.Content)
				for scanner.Scan() {
					bytesRead := scanner.Bytes()
					currentLine++

					possibleTerminator := trimNewline(bytesRead)
					if heredoc.Chomp {
						possibleTerminator = trimLeadingTabs(possibleTerminator)
					}
					if bytes.Equal(possibleTerminator, terminator) {
						terminated = true
						break
					}
					content.Write(bytesRead)
				}
				if !terminated {
					return nil, withLocation(errors.New("unterminated heredoc"), startLine, currentLine)
				}

				heredoc.Content = content.String()
				child.Heredocs = append(child.Heredocs, heredoc)
			}
		}

		root.AddChild(child, startLine, currentLine)
		comments = nil
	}

	if root.StartLine < 0 {
		return nil, withLocation(errors.New("file with no instructions"), currentLine, 0)
	}

	return &Result{
		AST:         root,
		Warnings:    warnings,
		EscapeToken: d.escapeToken,
	}, withLocation(handleScannerError(scanner.Err()), currentLine, 0)
}

// heredocFromMatch extracts a heredoc from a possible heredoc regex match.
func heredocFromMatch(match []string) (*Heredoc, error) {
	if len(match) == 0 {
		return nil, nil
	}

	fd, _ := strconv.ParseUint(match[1], 10, 0)
	chomp := match[2] == "-"
	rest := match[3]

	if len(rest) == 0 {
		return nil, nil
	}

	shlex := shell.NewLex('\\')
	shlex.SkipUnsetEnv = true

	// Attempt to parse both the heredoc both with *and* without quotes.
	// If there are quotes in one but not the other, then we know that some
	// part of the heredoc word is quoted, so we shouldn't expand the content.
	shlex.RawQuotes = false
	words, err := shlex.ProcessWords(rest, emptyEnvs{})
	if err != nil {
		return nil, err
	}
	// quick sanity check that rest is a single word
	if len(words) != 1 {
		return nil, nil
	}

	shlex.RawQuotes = true
	wordsRaw, err := shlex.ProcessWords(rest, emptyEnvs{})
	if err != nil {
		return nil, err
	}
	if len(wordsRaw) != len(words) {
		return nil, errors.Errorf("internal lexing of heredoc produced inconsistent results: %s", rest)
	}

	word := words[0]
	wordQuoteCount := strings.Count(word, `'`) + strings.Count(word, `"`)
	wordRaw := wordsRaw[0]
	wordRawQuoteCount := strings.Count(wordRaw, `'`) + strings.Count(wordRaw, `"`)

	expand := wordQuoteCount == wordRawQuoteCount

	return &Heredoc{
		Name:           word,
		Expand:         expand,
		Chomp:          chomp,
		FileDescriptor: uint(fd),
	}, nil
}

// ParseHeredoc parses a heredoc word from a target string, returning the
// components from the doc.
func ParseHeredoc(src string) (*Heredoc, error) {
	return heredocFromMatch(reHeredoc.FindStringSubmatch(src))
}

// MustParseHeredoc is a variant of ParseHeredoc that discards the error, if
// there was one present.
func MustParseHeredoc(src string) *Heredoc {
	heredoc, _ := ParseHeredoc(src)
	return heredoc
}

func heredocsFromLine(line string) ([]Heredoc, error) {
	shlex := shell.NewLex('\\')
	shlex.RawQuotes = true
	shlex.RawEscapes = true
	shlex.SkipUnsetEnv = true
	words, _ := shlex.ProcessWords(line, emptyEnvs{})

	var docs []Heredoc
	for _, word := range words {
		heredoc, err := ParseHeredoc(word)
		if err != nil {
			return nil, err
		}
		if heredoc != nil {
			docs = append(docs, *heredoc)
		}
	}
	return docs, nil
}

// ChompHeredocContent chomps leading tabs from the heredoc.
func ChompHeredocContent(src string) string {
	return reLeadingTabs.ReplaceAllString(src, "")
}

func trimComments(src []byte) []byte {
	if !isComment(src) {
		return src
	}
	return nil
}

func trimLeadingWhitespace(src []byte) []byte {
	return bytes.TrimLeftFunc(src, unicode.IsSpace)
}
func trimLeadingTabs(src []byte) []byte {
	return bytes.TrimLeft(src, "\t")
}
func trimNewline(src []byte) []byte {
	return bytes.TrimRight(src, "\r\n")
}

func isComment(line []byte) bool {
	line = trimLeadingWhitespace(line)
	return len(line) > 0 && line[0] == '#'
}

func isEmptyContinuationLine(line []byte) bool {
	return len(trimLeadingWhitespace(trimNewline(line))) == 0
}

func trimContinuationCharacter(line []byte, d *directives) ([]byte, bool) {
	if d.lineContinuationRegex.Match(line) {
		line = d.lineContinuationRegex.ReplaceAll(line, []byte("$1"))
		return line, false
	}
	return line, true
}

// TODO: remove stripLeftWhitespace after deprecation period. It seems silly
// to preserve whitespace on continuation lines. Why is that done?
func processLine(d *directives, token []byte, stripLeftWhitespace bool) ([]byte, bool, error) {
	token = trimNewline(token)
	if stripLeftWhitespace {
		token = trimLeadingWhitespace(token)
	}
	directiveOk, err := d.possibleParserDirective(token)
	return trimComments(token), directiveOk, err
}

// Variation of bufio.ScanLines that preserves the line endings
func scanLines(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	if i := bytes.IndexByte(data, '\n'); i >= 0 {
		return i + 1, data[0 : i+1], nil
	}
	if atEOF {
		return len(data), data, nil
	}
	return 0, nil, nil
}

func handleScannerError(err error) error {
	switch {
	case errors.Is(err, bufio.ErrTooLong):
		return errors.Errorf("dockerfile line greater than max allowed size of %d", bufio.MaxScanTokenSize-1)
	default:
		return err
	}
}

type emptyEnvs struct{}

func (emptyEnvs) Get(string) (string, bool) {
	return "", false
}

func (emptyEnvs) Keys() []string {
	return nil
}
