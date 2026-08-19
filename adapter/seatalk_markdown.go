package adapter

import (
	"bytes"
	"cmp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/yuin/goldmark"
	goldmarkast "github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	goldmarktext "github.com/yuin/goldmark/text"
)

func normalizeSeaTalkMarkdown(value string) string {
	if value == "" {
		return value
	}

	source := []byte(value)
	document := parseSeaTalkMarkdown(source)
	edits := collectSeaTalkMarkdownEdits(document, source)
	normalized := value
	if len(edits) != 0 {
		normalized = applySeaTalkMarkdownEdits(source, edits)
	}

	return escapeSeaTalkMarkdownOrderedSectionNumbers(normalized)
}

func escapeSeaTalkMarkdownOrderedSectionNumbers(value string) string {
	if value == "" {
		return value
	}

	source := []byte(value)
	document := parseSeaTalkMarkdown(source)
	edits := seaTalkMarkdownOrderedSectionNumberEdits(document, source)
	if len(edits) == 0 {
		return value
	}

	return applySeaTalkMarkdownEdits(source, edits)
}

// parseSeaTalkMarkdown recognizes the Markdown features SeaTalk supports.
// Tables must be parsed as top-level blocks so they are preserved intact when
// a response is normalized or split to meet SeaTalk's message size limit.
func parseSeaTalkMarkdown(source []byte) goldmarkast.Node {
	return goldmark.New(goldmark.WithExtensions(extension.Table)).Parser().Parse(goldmarktext.NewReader(source))
}

type seaTalkMarkdownEdit struct {
	start       int
	stop        int
	replacement string
}

func collectSeaTalkMarkdownEdits(document goldmarkast.Node, source []byte) []seaTalkMarkdownEdit {
	edits := make([]seaTalkMarkdownEdit, 0)

	_ = goldmarkast.Walk(document, func(node goldmarkast.Node, entering bool) (goldmarkast.WalkStatus, error) {
		if !entering {
			return goldmarkast.WalkContinue, nil
		}

		fencedCodeBlock, ok := node.(*goldmarkast.FencedCodeBlock)
		if ok {
			edit, ok := seaTalkMarkdownCodeFenceEdit(fencedCodeBlock, source)
			if ok {
				edits = append(edits, edit)
			}
		}

		emphasis, ok := node.(*goldmarkast.Emphasis)
		if ok {
			edit, ok := seaTalkMarkdownUnnestInlineLabelEdit(emphasis, source)
			if ok {
				edits = append(edits, edit)
			} else {
				edits = append(edits, seaTalkMarkdownItalicToBoldEdits(emphasis, source)...)
			}
		}

		textNode, ok := node.(*goldmarkast.Text)
		if ok {
			edit, ok := seaTalkMarkdownSoftLineBreakEdit(textNode, source)
			if ok {
				edits = append(edits, edit)
			}
		}

		list, ok := node.(*goldmarkast.List)
		if ok {
			if seaTalkMarkdownListIsNested(list) {
				edits = append(edits, seaTalkMarkdownNestedListIndentationEdits(list, source)...)
			}
			edits = append(edits, seaTalkMarkdownUnorderedListMarkerEdits(list, source)...)
			edits = append(edits, seaTalkMarkdownListMarkerSpacingEdits(list, source)...)
			edits = append(edits, seaTalkMarkdownPromoteLooseTopLevelUnorderedListItemContinuationEdits(list, source)...)
			edits = append(edits, seaTalkMarkdownListSpacingEdits(list, source)...)
		}

		return goldmarkast.WalkContinue, nil
	})

	return edits
}

// seaTalkMarkdownOrderedSectionNumberEdits escapes loose top-level ordered-list
// markers so SeaTalk does not misrender them as numbered sections.
func seaTalkMarkdownOrderedSectionNumberEdits(document goldmarkast.Node, source []byte) []seaTalkMarkdownEdit {
	edits := make([]seaTalkMarkdownEdit, 0)

	_ = goldmarkast.Walk(document, func(node goldmarkast.Node, entering bool) (goldmarkast.WalkStatus, error) {
		if !entering {
			return goldmarkast.WalkContinue, nil
		}

		list, ok := node.(*goldmarkast.List)
		if !ok || !list.IsOrdered() || !seaTalkMarkdownListIsTopLevel(list) {
			return goldmarkast.WalkContinue, nil
		}

		if seaTalkMarkdownListIsStrictTight(list) {
			// SeaTalk Markdown doesn't support Markdown headings, and the model may use a top-level heading-like line,
			// which looks like a single-item ordered list.
			// So escaping the marker preserves the line as plain text instead of an ordered list item.
			if list.ChildCount() != 1 {
				return goldmarkast.WalkContinue, nil
			}

			listItem, ok := list.FirstChild().(*goldmarkast.ListItem)
			if !ok || !seaTalkMarkdownListItemIsSimple(listItem) {
				return goldmarkast.WalkContinue, nil
			}

			edit, ok := seaTalkMarkdownOrderedListItemMarkerEscapeEdit(listItem, source)
			if ok {
				edits = append(edits, edit)
			}

			return goldmarkast.WalkContinue, nil
		}

		edits = append(edits, seaTalkMarkdownPromoteLooseTopLevelOrderedListItemContinuationEdits(list, source)...)

		for item := list.FirstChild(); item != nil; item = item.NextSibling() {
			listItem, ok := item.(*goldmarkast.ListItem)
			if !ok {
				continue
			}

			edit, ok := seaTalkMarkdownOrderedListItemMarkerEscapeEdit(listItem, source)
			if ok {
				edits = append(edits, edit)
			}
		}

		return goldmarkast.WalkContinue, nil
	})

	return edits
}

func seaTalkMarkdownListIsStrictTight(list *goldmarkast.List) bool {
	if list == nil || !list.IsTight {
		return false
	}

	for item := list.FirstChild(); item != nil; item = item.NextSibling() {
		listItem, ok := item.(*goldmarkast.ListItem)
		if !ok {
			return false
		}
		if !seaTalkMarkdownListItemIsStrictTight(listItem) {
			return false
		}
	}

	return true
}

func seaTalkMarkdownListItemIsStrictTight(listItem *goldmarkast.ListItem) bool {
	if listItem == nil {
		return false
	}

	hasNestedList := false
	hasOtherBlock := false
	for child := listItem.FirstChild(); child != nil; child = child.NextSibling() {
		if _, ok := child.(*goldmarkast.List); ok {
			hasNestedList = true
			continue
		}

		hasOtherBlock = true
		if !seaTalkMarkdownListItemBlockIsSingleLine(child) {
			return false
		}
	}

	if hasNestedList && hasOtherBlock {
		return false
	}

	return hasNestedList || hasOtherBlock
}

func seaTalkMarkdownListItemIsSimple(listItem *goldmarkast.ListItem) bool {
	if listItem == nil || listItem.ChildCount() != 1 {
		return false
	}

	return seaTalkMarkdownListItemBlockIsSingleLine(listItem.FirstChild())
}

func seaTalkMarkdownListItemBlockIsSingleLine(node goldmarkast.Node) bool {
	if node == nil {
		return false
	}

	switch value := node.(type) {
	case *goldmarkast.TextBlock:
		lines := value.Lines()
		return lines != nil && lines.Len() == 1
	case *goldmarkast.Paragraph:
		lines := value.Lines()
		return lines != nil && lines.Len() == 1
	default:
		return false
	}
}

// seaTalkMarkdownPromoteLooseTopLevelOrderedListItemContinuationEdits shifts
// continuation lines inside loose top-level ordered items one level left.
func seaTalkMarkdownPromoteLooseTopLevelOrderedListItemContinuationEdits(list *goldmarkast.List, source []byte) []seaTalkMarkdownEdit {
	edits := make([]seaTalkMarkdownEdit, 0)
	if list == nil {
		return edits
	}

	for item := list.FirstChild(); item != nil; item = item.NextSibling() {
		listItem, ok := item.(*goldmarkast.ListItem)
		if !ok {
			continue
		}

		edits = append(edits, seaTalkMarkdownPromoteListItemContinuationLineEdits(listItem, source)...)
	}

	return edits
}

// seaTalkMarkdownPromoteListItemContinuationLineEdits rewrites every
// continuation line after the list item's first line with one less indent
// level, removing either one leading tab or up to four leading spaces.
func seaTalkMarkdownPromoteListItemContinuationLineEdits(listItem *goldmarkast.ListItem, source []byte) []seaTalkMarkdownEdit {
	edits := make([]seaTalkMarkdownEdit, 0)
	if listItem == nil {
		return edits
	}

	firstLineStart := seaTalkMarkdownLineStart(source, listItem.Pos())
	position := seaTalkMarkdownLineEnd(source, firstLineStart)
	if position < len(source) && source[position] == '\n' {
		position++
	}

	stop := seaTalkMarkdownNodeStop(listItem, source)
	for position < stop {
		lineStart := position
		edit, ok := seaTalkMarkdownPromoteListItemLineIndentationEdit(lineStart, source)
		if ok {
			edits = append(edits, edit)
		}

		position = seaTalkMarkdownLineEnd(source, lineStart)
		if position < len(source) && source[position] == '\n' {
			position++
		}
	}

	return edits
}

func seaTalkMarkdownPromoteListItemLineIndentationEdit(lineStart int, source []byte) (seaTalkMarkdownEdit, bool) {
	if lineStart < 0 || lineStart >= len(source) {
		return seaTalkMarkdownEdit{}, false
	}

	lineEnd := seaTalkMarkdownLineEnd(source, lineStart)
	if lineEnd <= lineStart {
		return seaTalkMarkdownEdit{}, false
	}

	switch source[lineStart] {
	case '\t':
		return seaTalkMarkdownEdit{
			start:       lineStart,
			stop:        lineStart + 1,
			replacement: "",
		}, true
	case ' ':
		stop := lineStart
		for stop < lineEnd && stop-lineStart < 4 && source[stop] == ' ' {
			stop++
		}
		if stop == lineStart {
			return seaTalkMarkdownEdit{}, false
		}
		return seaTalkMarkdownEdit{
			start:       lineStart,
			stop:        stop,
			replacement: "",
		}, true
	default:
		return seaTalkMarkdownEdit{}, false
	}
}

func seaTalkMarkdownOrderedListItemMarkerEscapeEdit(listItem *goldmarkast.ListItem, source []byte) (seaTalkMarkdownEdit, bool) {
	if listItem == nil {
		return seaTalkMarkdownEdit{}, false
	}

	lineStart := seaTalkMarkdownLineStart(source, listItem.Pos())
	lineEnd := seaTalkMarkdownLineEnd(source, lineStart)
	if lineStart < 0 || lineStart >= len(source) || lineEnd <= lineStart {
		return seaTalkMarkdownEdit{}, false
	}

	position := lineStart
	for position < lineEnd && source[position] == ' ' {
		position++
	}

	numberStart := position
	for position < lineEnd && source[position] >= '0' && source[position] <= '9' {
		position++
	}
	if position == numberStart || position >= lineEnd || source[position] != '.' {
		return seaTalkMarkdownEdit{}, false
	}

	return seaTalkMarkdownEdit{
		start:       position,
		stop:        position + 1,
		replacement: `\.`,
	}, true
}

// seaTalkMarkdownListMarkerSpacingEdits normalizes whitespace after list
// markers so SeaTalk sees a single-space separator consistently.
func seaTalkMarkdownListMarkerSpacingEdits(list *goldmarkast.List, source []byte) []seaTalkMarkdownEdit {
	edits := make([]seaTalkMarkdownEdit, 0)
	if list == nil {
		return edits
	}

	for item := list.FirstChild(); item != nil; item = item.NextSibling() {
		listItem, ok := item.(*goldmarkast.ListItem)
		if !ok {
			continue
		}

		edit, ok := seaTalkMarkdownListItemMarkerSpacingEdit(listItem, source)
		if ok {
			edits = append(edits, edit)
		}
	}

	return edits
}

func seaTalkMarkdownListItemMarkerSpacingEdit(listItem *goldmarkast.ListItem, source []byte) (seaTalkMarkdownEdit, bool) {
	if listItem == nil {
		return seaTalkMarkdownEdit{}, false
	}

	lineStart := seaTalkMarkdownLineStart(source, listItem.Pos())
	lineEnd := seaTalkMarkdownLineEnd(source, lineStart)
	if lineStart < 0 || lineStart >= len(source) || lineEnd <= lineStart {
		return seaTalkMarkdownEdit{}, false
	}

	markerStart := lineStart
	for markerStart < lineEnd && (source[markerStart] == ' ' || source[markerStart] == '\t') {
		markerStart++
	}
	if markerStart >= lineEnd {
		return seaTalkMarkdownEdit{}, false
	}

	markerStop := markerStart
	switch source[markerStart] {
	case '-', '*', '+':
		markerStop++
	default:
		for markerStop < lineEnd && source[markerStop] >= '0' && source[markerStop] <= '9' {
			markerStop++
		}
		if markerStop == markerStart || markerStop >= lineEnd {
			return seaTalkMarkdownEdit{}, false
		}
		if source[markerStop] != '.' && source[markerStop] != ')' {
			return seaTalkMarkdownEdit{}, false
		}
		markerStop++
	}

	spaceStart := markerStop
	spaceStop := spaceStart
	for spaceStop < lineEnd && (source[spaceStop] == ' ' || source[spaceStop] == '\t') {
		spaceStop++
	}
	if spaceStop == spaceStart {
		return seaTalkMarkdownEdit{}, false
	}
	if spaceStop-spaceStart == 1 && source[spaceStart] == ' ' {
		return seaTalkMarkdownEdit{}, false
	}

	return seaTalkMarkdownEdit{
		start:       spaceStart,
		stop:        spaceStop,
		replacement: " ",
	}, true
}

// seaTalkMarkdownUnorderedListMarkerEdits normalizes '*' unordered list markers
// to '-' because SeaTalk only supports that marker reliably.
func seaTalkMarkdownUnorderedListMarkerEdits(list *goldmarkast.List, source []byte) []seaTalkMarkdownEdit {
	edits := make([]seaTalkMarkdownEdit, 0)
	if list == nil || list.IsOrdered() {
		return edits
	}

	for item := list.FirstChild(); item != nil; item = item.NextSibling() {
		listItem, ok := item.(*goldmarkast.ListItem)
		if !ok {
			continue
		}

		edit, ok := seaTalkMarkdownUnorderedListItemMarkerEdit(listItem, source)
		if ok {
			edits = append(edits, edit)
		}
	}

	return edits
}

func seaTalkMarkdownUnorderedListItemMarkerEdit(listItem *goldmarkast.ListItem, source []byte) (seaTalkMarkdownEdit, bool) {
	if listItem == nil {
		return seaTalkMarkdownEdit{}, false
	}

	lineStart := seaTalkMarkdownLineStart(source, listItem.Pos())
	lineEnd := seaTalkMarkdownLineEnd(source, lineStart)
	if lineStart < 0 || lineStart >= len(source) || lineEnd <= lineStart {
		return seaTalkMarkdownEdit{}, false
	}

	markerStart := lineStart
	for markerStart < lineEnd && (source[markerStart] == ' ' || source[markerStart] == '\t') {
		markerStart++
	}
	if markerStart >= lineEnd {
		return seaTalkMarkdownEdit{}, false
	}

	switch source[markerStart] {
	case '*':
		return seaTalkMarkdownEdit{
			start:       markerStart,
			stop:        markerStart + 1,
			replacement: "-",
		}, true
	default:
		return seaTalkMarkdownEdit{}, false
	}
}

// seaTalkMarkdownItalicToBoldEdits rewrites tight inline italic into bold so
// SeaTalk renders emphasis reliably even when spacing is missing.
func seaTalkMarkdownItalicToBoldEdits(emphasis *goldmarkast.Emphasis, source []byte) []seaTalkMarkdownEdit {
	if emphasis == nil || emphasis.Level != 1 {
		return nil
	}
	if seaTalkMarkdownHasEmphasisAncestor(emphasis) {
		return nil
	}
	if seaTalkMarkdownIsBoldItalicEmphasis(emphasis) {
		return nil
	}

	opening := emphasis.Pos()
	if opening < 0 || opening >= len(source) {
		return nil
	}
	if source[opening] != '*' && source[opening] != '_' {
		return nil
	}

	closing, ok := seaTalkMarkdownItalicClosingDelimiterPosition(emphasis, source)
	if !ok {
		return nil
	}
	if source[closing] != source[opening] {
		return nil
	}
	if seaTalkMarkdownHasWhitespaceBefore(source, opening) && seaTalkMarkdownHasWhitespaceAfter(source, closing+1) {
		return nil
	}

	delimiter := source[opening]
	return []seaTalkMarkdownEdit{
		{start: opening, stop: opening + 1, replacement: string([]byte{delimiter, delimiter})},
		{start: closing, stop: closing + 1, replacement: string([]byte{delimiter, delimiter})},
	}
}

func seaTalkMarkdownIsBoldItalicEmphasis(emphasis *goldmarkast.Emphasis) bool {
	if emphasis == nil || emphasis.Level != 1 || emphasis.ChildCount() != 1 {
		return false
	}

	child, ok := emphasis.FirstChild().(*goldmarkast.Emphasis)
	return ok && child.Level == 2
}

func seaTalkMarkdownHasEmphasisAncestor(node goldmarkast.Node) bool {
	for parent := node.Parent(); parent != nil; parent = parent.Parent() {
		if _, ok := parent.(*goldmarkast.Emphasis); ok {
			return true
		}
	}

	return false
}

// seaTalkMarkdownUnnestInlineLabelEdit splits emphasis around nested inline
// labels such as code spans so SeaTalk does not receive unsupported nesting.
func seaTalkMarkdownUnnestInlineLabelEdit(emphasis *goldmarkast.Emphasis, source []byte) (seaTalkMarkdownEdit, bool) {
	if emphasis == nil || emphasis.Level < 1 {
		return seaTalkMarkdownEdit{}, false
	}

	opening, delimiterWidth, closing, ok := seaTalkMarkdownEffectiveEmphasisBounds(emphasis, source)
	if !ok || opening < 0 || opening+delimiterWidth > len(source) {
		return seaTalkMarkdownEdit{}, false
	}
	if closing < opening+delimiterWidth || closing+delimiterWidth > len(source) {
		return seaTalkMarkdownEdit{}, false
	}

	contentStart := opening + delimiterWidth
	contentEnd := closing
	cursor := contentStart
	hasNestedInline := false

	var builder strings.Builder
	for child := emphasis.FirstChild(); child != nil; child = child.NextSibling() {
		if !seaTalkMarkdownIsUnsupportedNestedInline(child) {
			continue
		}

		childStart := child.Pos()
		childStop := seaTalkMarkdownInlineNodeStop(child, source)
		if emphasisChild, ok := child.(*goldmarkast.Emphasis); ok && childStop > contentEnd {
			if adjustedStop, ok := seaTalkMarkdownSharedTrailingEmphasisStop(emphasisChild, source, contentEnd); ok {
				childStop = adjustedStop
			}
		}
		if childStart < cursor || childStop < childStart || childStop > contentEnd {
			continue
		}

		hasNestedInline = true
		leftStop := seaTalkMarkdownTrimRightWhitespaceBoundary(source, cursor, childStart)
		rightStart := seaTalkMarkdownTrimLeftWhitespaceBoundary(source, childStop, contentEnd)

		seaTalkMarkdownAppendWrappedEmphasisSegment(&builder, emphasis, source, cursor, leftStop)
		if seaTalkMarkdownNeedsInlineChildLeadingSpace(source, opening, delimiterWidth, leftStop, childStart) {
			builder.WriteByte(' ')
		}
		builder.Write(source[leftStop:rightStart])
		if seaTalkMarkdownNeedsInlineChildTrailingSpace(source, closing, rightStart, childStop) {
			builder.WriteByte(' ')
		}
		cursor = rightStart
	}

	if !hasNestedInline {
		return seaTalkMarkdownEdit{}, false
	}

	seaTalkMarkdownAppendWrappedEmphasisSegment(&builder, emphasis, source, cursor, contentEnd)

	return seaTalkMarkdownEdit{
		start:       opening,
		stop:        closing + delimiterWidth,
		replacement: builder.String(),
	}, true
}

func seaTalkMarkdownIsUnsupportedNestedInline(node goldmarkast.Node) bool {
	switch node.(type) {
	case *goldmarkast.CodeSpan, *goldmarkast.Emphasis:
		return true
	default:
		return false
	}
}

func seaTalkMarkdownAppendWrappedEmphasisSegment(
	builder *strings.Builder,
	emphasis *goldmarkast.Emphasis,
	source []byte,
	start int,
	stop int,
) {
	if builder == nil || emphasis == nil || start >= stop {
		return
	}

	delimiter := seaTalkMarkdownWrappedEmphasisDelimiter(emphasis, source, start, stop)
	content := string(source[start:stop])
	builder.WriteString(delimiter)
	builder.WriteString(content)
	builder.WriteString(delimiter)
}

func seaTalkMarkdownWrappedEmphasisDelimiter(
	emphasis *goldmarkast.Emphasis,
	source []byte,
	start int,
	stop int,
) string {
	if emphasis == nil || emphasis.Level < 1 {
		return ""
	}

	opening, delimiterWidth, closing, ok := seaTalkMarkdownEffectiveEmphasisBounds(emphasis, source)
	if !ok || opening < 0 || opening+delimiterWidth > len(source) {
		return ""
	}

	delimiter := string(source[opening : opening+delimiterWidth])
	if delimiterWidth != 1 || delimiter == "" {
		return delimiter
	}

	segmentHasWhitespaceBefore := seaTalkMarkdownHasWhitespaceBefore(source, start)
	segmentHasWhitespaceAfter := seaTalkMarkdownHasWhitespaceAfter(source, stop)

	if start == opening+delimiterWidth {
		segmentHasWhitespaceBefore = seaTalkMarkdownHasWhitespaceBefore(source, opening)
	}
	if stop == closing {
		segmentHasWhitespaceAfter = seaTalkMarkdownHasWhitespaceAfter(source, closing+delimiterWidth)
	}

	if segmentHasWhitespaceBefore && segmentHasWhitespaceAfter {
		return delimiter
	}

	return delimiter + delimiter
}

func seaTalkMarkdownEffectiveEmphasisBounds(emphasis *goldmarkast.Emphasis, source []byte) (int, int, int, bool) {
	if emphasis == nil || emphasis.Level < 1 {
		return 0, 0, 0, false
	}

	opening := emphasis.Pos()
	delimiterWidth := emphasis.Level
	target := emphasis
	if outer, ok := seaTalkMarkdownBoldItalicOuterForInner(emphasis); ok {
		opening = outer.Pos()
		delimiterWidth = outer.Level + emphasis.Level
		target = outer
	}

	closing, ok := seaTalkMarkdownItalicClosingDelimiterPosition(target, source)
	if !ok {
		return 0, 0, 0, false
	}

	return opening, delimiterWidth, closing, true
}

func seaTalkMarkdownBoldItalicOuterForInner(emphasis *goldmarkast.Emphasis) (*goldmarkast.Emphasis, bool) {
	if emphasis == nil || emphasis.Level != 2 {
		return nil, false
	}

	parent, ok := emphasis.Parent().(*goldmarkast.Emphasis)
	if !ok || parent.Level != 1 || parent.ChildCount() != 1 || parent.FirstChild() != emphasis {
		return nil, false
	}

	return parent, true
}

func seaTalkMarkdownInlineNodeStop(node goldmarkast.Node, source []byte) int {
	switch value := node.(type) {
	case *goldmarkast.CodeSpan:
		stop := seaTalkMarkdownNodeStop(node, source)
		return min(len(source), stop+seaTalkMarkdownRepeatedMarkerWidth(source, value.Pos(), '`'))
	case *goldmarkast.Emphasis:
		opening, delimiterWidth, closing, ok := seaTalkMarkdownEffectiveEmphasisBounds(value, source)
		if !ok {
			stop := seaTalkMarkdownNodeStop(node, source)
			return min(len(source), stop+value.Level)
		}
		if closing < opening+delimiterWidth {
			return min(len(source), closing+value.Level)
		}
		return min(len(source), closing+delimiterWidth)
	default:
		return seaTalkMarkdownNodeStop(node, source)
	}
}

func seaTalkMarkdownSharedTrailingEmphasisStop(
	emphasis *goldmarkast.Emphasis,
	source []byte,
	contentEnd int,
) (int, bool) {
	if emphasis == nil || contentEnd < 0 {
		return 0, false
	}

	closing, ok := seaTalkMarkdownItalicClosingDelimiterPosition(emphasis, source)
	if !ok || closing != contentEnd {
		return 0, false
	}

	return contentEnd, true
}

func seaTalkMarkdownRepeatedMarkerWidth(source []byte, start int, marker byte) int {
	if start < 0 || start >= len(source) || source[start] != marker {
		return 0
	}

	stop := start
	for stop < len(source) && source[stop] == marker {
		stop++
	}

	return stop - start
}

func seaTalkMarkdownTrimRightWhitespaceBoundary(source []byte, start int, stop int) int {
	if start < 0 {
		start = 0
	}
	if stop > len(source) {
		stop = len(source)
	}

	position := stop
	for position > start {
		r, size := utf8.DecodeLastRune(source[start:position])
		if r == utf8.RuneError || !unicode.IsSpace(r) {
			break
		}
		position -= size
	}

	return position
}

func seaTalkMarkdownTrimLeftWhitespaceBoundary(source []byte, start int, stop int) int {
	if start < 0 {
		start = 0
	}
	if stop > len(source) {
		stop = len(source)
	}

	position := start
	for position < stop {
		r, size := utf8.DecodeRune(source[position:stop])
		if r == utf8.RuneError || !unicode.IsSpace(r) {
			break
		}
		position += size
	}

	return position
}

func seaTalkMarkdownNeedsInlineChildLeadingSpace(
	source []byte,
	opening int,
	delimiterWidth int,
	leftStop int,
	childStart int,
) bool {
	if leftStop != childStart || leftStop <= opening+delimiterWidth || leftStop > len(source) {
		return false
	}

	r, _ := utf8.DecodeLastRune(source[:leftStop])
	return r != utf8.RuneError && r != '\n' && !unicode.IsSpace(r)
}

func seaTalkMarkdownNeedsInlineChildTrailingSpace(
	source []byte,
	closing int,
	rightStart int,
	childStop int,
) bool {
	if rightStart != childStop || rightStart < 0 || rightStart >= closing || rightStart >= len(source) {
		return false
	}

	r, _ := utf8.DecodeRune(source[rightStart:])
	return r != utf8.RuneError && r != '\n' && !unicode.IsSpace(r)
}

// seaTalkMarkdownSoftLineBreakEdit replaces an ordered-list soft break with a
// visible continuation marker so the wrapped line is preserved in SeaTalk.
func seaTalkMarkdownSoftLineBreakEdit(textNode *goldmarkast.Text, source []byte) (seaTalkMarkdownEdit, bool) {
	if textNode == nil || !textNode.SoftLineBreak() || !seaTalkMarkdownListItemNeedsCompaction(textNode) {
		return seaTalkMarkdownEdit{}, false
	}

	nextText, ok := textNode.NextSibling().(*goldmarkast.Text)
	if !ok {
		return seaTalkMarkdownEdit{}, false
	}

	start := textNode.Segment.Stop
	stop := nextText.Pos()
	if start < 0 || stop < start || stop > len(source) {
		return seaTalkMarkdownEdit{}, false
	}

	return seaTalkMarkdownEdit{
		start:       start,
		stop:        stop,
		replacement: " ",
	}, true
}

func seaTalkMarkdownItalicClosingDelimiterPosition(emphasis *goldmarkast.Emphasis, source []byte) (int, bool) {
	if emphasis == nil || emphasis.LastChild() == nil {
		return 0, false
	}

	stop := seaTalkMarkdownNodeStop(emphasis.LastChild(), source)
	if stop <= emphasis.Pos() || stop >= len(source) {
		return 0, false
	}

	return stop, true
}

func seaTalkMarkdownHasWhitespaceBefore(source []byte, position int) bool {
	if position <= 0 {
		return true
	}

	r, _ := utf8.DecodeLastRune(source[:position])
	if r == utf8.RuneError {
		return false
	}

	return unicode.IsSpace(r) || seaTalkMarkdownIsASCIIPunctuationBoundary(r)
}

func seaTalkMarkdownHasWhitespaceAfter(source []byte, position int) bool {
	if position >= len(source) {
		return true
	}

	r, _ := utf8.DecodeRune(source[position:])
	if r == utf8.RuneError {
		return false
	}

	return unicode.IsSpace(r) || seaTalkMarkdownIsASCIIPunctuationBoundary(r)
}

func seaTalkMarkdownIsASCIIPunctuationBoundary(r rune) bool {
	if r < '!' || r > '~' {
		return false
	}

	return !('0' <= r && r <= '9') && !('a' <= r && r <= 'z') && !('A' <= r && r <= 'Z')
}

func seaTalkMarkdownNodeStop(node goldmarkast.Node, source []byte) int {
	if node == nil {
		return -1
	}
	if textNode, ok := node.(*goldmarkast.Text); ok {
		return textNode.Segment.Stop
	}
	if node.LastChild() != nil {
		return seaTalkMarkdownNodeStop(node.LastChild(), source)
	}
	if node.Type() == goldmarkast.TypeBlock {
		lines := node.Lines()
		if lines != nil && lines.Len() > 0 {
			return lines.At(lines.Len() - 1).Stop
		}
	}
	if node.Pos() < 0 {
		return -1
	}
	if len(source) <= node.Pos() {
		return node.Pos()
	}

	return node.Pos() + len(source[node.Pos():node.Pos()+1])
}

func seaTalkMarkdownListIsNested(list *goldmarkast.List) bool {
	if list == nil {
		return false
	}

	_, ok := list.Parent().(*goldmarkast.ListItem)
	return ok
}

// seaTalkMarkdownCodeFenceEdit strips fenced-code info strings because SeaTalk
// supports plain fences more reliably than language-tagged fences.
func seaTalkMarkdownCodeFenceEdit(node *goldmarkast.FencedCodeBlock, source []byte) (seaTalkMarkdownEdit, bool) {
	if node == nil {
		return seaTalkMarkdownEdit{}, false
	}

	start := node.Pos()
	if start < 0 || start >= len(source) {
		return seaTalkMarkdownEdit{}, false
	}

	marker := source[start]
	if marker != '`' && marker != '~' {
		return seaTalkMarkdownEdit{}, false
	}

	stop := start
	for stop < len(source) && source[stop] == marker {
		stop++
	}

	lineEnd := seaTalkMarkdownLineEnd(source, start)
	if stop >= lineEnd {
		return seaTalkMarkdownEdit{}, false
	}

	return seaTalkMarkdownEdit{start: stop, stop: lineEnd}, true
}

func seaTalkMarkdownListNeedsCompaction(list *goldmarkast.List) bool {
	if list == nil {
		return false
	}
	return !seaTalkMarkdownListIsTopLevel(list)
}

// seaTalkMarkdownPromoteLooseTopLevelUnorderedListItemContinuationEdits shifts
// continuation lines inside loose top-level unordered items one level left.
func seaTalkMarkdownPromoteLooseTopLevelUnorderedListItemContinuationEdits(list *goldmarkast.List, source []byte) []seaTalkMarkdownEdit {
	edits := make([]seaTalkMarkdownEdit, 0)
	if list == nil || list.IsOrdered() || !seaTalkMarkdownListIsTopLevel(list) || seaTalkMarkdownListIsStrictTight(list) {
		return edits
	}

	for item := list.FirstChild(); item != nil; item = item.NextSibling() {
		listItem, ok := item.(*goldmarkast.ListItem)
		if !ok {
			continue
		}

		edits = append(edits, seaTalkMarkdownPromoteLooseTopLevelUnorderedListItemChildEdits(listItem, source)...)
	}

	return edits
}

func seaTalkMarkdownPromoteLooseTopLevelUnorderedListItemChildEdits(listItem *goldmarkast.ListItem, source []byte) []seaTalkMarkdownEdit {
	edits := make([]seaTalkMarkdownEdit, 0)
	if listItem == nil {
		return edits
	}

	firstChild := true
	for child := listItem.FirstChild(); child != nil; child = child.NextSibling() {
		if _, ok := child.(*goldmarkast.List); ok {
			firstChild = false
			continue
		}

		edits = append(edits, seaTalkMarkdownPromoteNodeContinuationLineEdits(child, source, !firstChild)...)
		firstChild = false
	}

	return edits
}

func seaTalkMarkdownPromoteNodeContinuationLineEdits(node goldmarkast.Node, source []byte, includeFirstLine bool) []seaTalkMarkdownEdit {
	edits := make([]seaTalkMarkdownEdit, 0)
	if node == nil {
		return edits
	}

	lineStart := seaTalkMarkdownLineStart(source, node.Pos())
	if lineStart < 0 || lineStart >= len(source) {
		return edits
	}

	position := lineStart
	if !includeFirstLine {
		position = seaTalkMarkdownLineEnd(source, lineStart)
		if position < len(source) && source[position] == '\n' {
			position++
		}
	}

	stop := seaTalkMarkdownNodeStop(node, source)
	if _, ok := node.(*goldmarkast.FencedCodeBlock); ok {
		stop = seaTalkMarkdownFencedCodeBlockStop(node, source)
	}
	for position < stop {
		edit, ok := seaTalkMarkdownPromoteListItemLineIndentationEdit(position, source)
		if ok {
			edits = append(edits, edit)
		}

		position = seaTalkMarkdownLineEnd(source, position)
		if position < len(source) && source[position] == '\n' {
			position++
		}
	}

	return edits
}

func seaTalkMarkdownFencedCodeBlockStop(node goldmarkast.Node, source []byte) int {
	stop := seaTalkMarkdownNodeStop(node, source)
	if stop < 0 {
		return stop
	}

	lineStart := seaTalkMarkdownLineStart(source, stop)
	lineEnd := seaTalkMarkdownLineEnd(source, lineStart)
	if lineEnd < len(source) {
		lineEnd++
	}

	return lineEnd
}

func seaTalkMarkdownListIsTopLevel(list *goldmarkast.List) bool {
	if list == nil {
		return false
	}

	_, ok := list.Parent().(*goldmarkast.ListItem)
	return !ok
}

func seaTalkMarkdownListItemNeedsCompaction(node goldmarkast.Node) bool {
	for parent := node.Parent(); parent != nil; parent = parent.Parent() {
		list, ok := parent.(*goldmarkast.List)
		if ok {
			return seaTalkMarkdownListNeedsCompaction(list)
		}
	}

	return false
}

// seaTalkMarkdownListSpacingEdits removes blank lines between sibling list
// items so SeaTalk keeps ordered-list contexts compact.
func seaTalkMarkdownListSpacingEdits(list *goldmarkast.List, source []byte) []seaTalkMarkdownEdit {
	edits := make([]seaTalkMarkdownEdit, 0)
	if list == nil || !seaTalkMarkdownListNeedsCompaction(list) {
		return edits
	}

	for item := list.FirstChild(); item != nil; item = item.NextSibling() {
		listItem, ok := item.(*goldmarkast.ListItem)
		if !ok || listItem.PreviousSibling() == nil {
			continue
		}

		edit, ok := seaTalkMarkdownBlankLineEditBeforePosition(source, listItem.Pos())
		if ok {
			edits = append(edits, edit)
		}
	}

	return edits
}

// seaTalkMarkdownNestedListIndentationEdits normalizes nested list indentation
// to four spaces, matching the layout SeaTalk handles reliably.
func seaTalkMarkdownNestedListIndentationEdits(list *goldmarkast.List, source []byte) []seaTalkMarkdownEdit {
	edits := make([]seaTalkMarkdownEdit, 0)
	if list == nil {
		return edits
	}

	parentListItem, ok := list.Parent().(*goldmarkast.ListItem)
	if !ok {
		return edits
	}

	parentActualIndent := seaTalkMarkdownListItemIndentation(parentListItem, source)
	parentNormalizedIndent := seaTalkMarkdownNormalizedListItemIndentation(parentListItem, source)

	for item := list.FirstChild(); item != nil; item = item.NextSibling() {
		listItem, ok := item.(*goldmarkast.ListItem)
		if !ok {
			continue
		}

		lineStart := seaTalkMarkdownLineStart(source, listItem.Pos())
		indentStop, actualIndent := seaTalkMarkdownLineIndentationSpan(source, lineStart)
		if actualIndent < parentActualIndent {
			continue
		}

		relativeIndent := actualIndent - parentActualIndent
		if relativeIndent != 2 && relativeIndent != 3 {
			continue
		}

		desiredIndent := parentNormalizedIndent + 4
		if desiredIndent == actualIndent {
			continue
		}

		edits = append(edits, seaTalkMarkdownEdit{
			start:       lineStart,
			stop:        indentStop,
			replacement: strings.Repeat(" ", desiredIndent),
		})
	}

	return edits
}

func seaTalkMarkdownListItemIndentation(listItem *goldmarkast.ListItem, source []byte) int {
	if listItem == nil {
		return 0
	}

	lineStart := seaTalkMarkdownLineStart(source, listItem.Pos())
	_, indent := seaTalkMarkdownLineIndentationSpan(source, lineStart)

	return indent
}

func seaTalkMarkdownLineIndentationSpan(source []byte, lineStart int) (int, int) {
	if lineStart < 0 {
		lineStart = 0
	}
	if lineStart > len(source) {
		lineStart = len(source)
	}

	lineEnd := seaTalkMarkdownLineEnd(source, lineStart)
	position := lineStart
	indentWidth := 0
	for position < lineEnd {
		switch source[position] {
		case ' ':
			indentWidth++
		case '\t':
			indentWidth += 4
		default:
			return position, indentWidth
		}
		position++
	}

	return position, indentWidth
}

func seaTalkMarkdownNormalizedListItemIndentation(listItem *goldmarkast.ListItem, source []byte) int {
	if listItem == nil {
		return 0
	}

	parentList, ok := listItem.Parent().(*goldmarkast.List)
	if !ok || seaTalkMarkdownListIsTopLevel(parentList) {
		return seaTalkMarkdownListItemIndentation(listItem, source)
	}

	parentListItem, ok := parentList.Parent().(*goldmarkast.ListItem)
	if !ok {
		return seaTalkMarkdownListItemIndentation(listItem, source)
	}

	parentActualIndent := seaTalkMarkdownListItemIndentation(parentListItem, source)
	actualIndent := seaTalkMarkdownListItemIndentation(listItem, source)
	relativeIndent := actualIndent - parentActualIndent
	if relativeIndent == 2 || relativeIndent == 3 {
		relativeIndent = 4
	}

	return seaTalkMarkdownNormalizedListItemIndentation(parentListItem, source) + relativeIndent
}

// seaTalkMarkdownBlankLineEditBeforePosition removes the contiguous blank lines
// immediately before a target position.
func seaTalkMarkdownBlankLineEditBeforePosition(source []byte, position int) (seaTalkMarkdownEdit, bool) {
	lineStart := seaTalkMarkdownLineStart(source, position)
	blankStart := lineStart
	for blankStart > 0 {
		prevLineStart := seaTalkMarkdownPreviousLineStart(source, blankStart)
		if prevLineStart < 0 {
			break
		}

		prevLineEnd := blankStart - 1
		line := source[prevLineStart:prevLineEnd]
		if len(bytes.TrimSpace(line)) != 0 {
			break
		}
		blankStart = prevLineStart
	}

	if blankStart >= lineStart {
		return seaTalkMarkdownEdit{}, false
	}

	return seaTalkMarkdownEdit{start: blankStart, stop: lineStart}, true
}

func applySeaTalkMarkdownEdits(source []byte, edits []seaTalkMarkdownEdit) string {
	if len(edits) == 0 {
		return string(source)
	}

	slices.SortFunc(edits, func(left, right seaTalkMarkdownEdit) int {
		if left.start != right.start {
			return cmp.Compare(right.start, left.start)
		}
		return cmp.Compare(right.stop, left.stop)
	})

	output := append([]byte(nil), source...)
	for _, edit := range edits {
		if edit.start < 0 || edit.stop < edit.start || edit.stop > len(output) {
			continue
		}
		output = append(output[:edit.start], append([]byte(edit.replacement), output[edit.stop:]...)...)
	}

	return string(output)
}

func seaTalkMarkdownLineStart(source []byte, position int) int {
	if position <= 0 {
		return 0
	}
	if position > len(source) {
		position = len(source)
	}

	for position > 0 && source[position-1] != '\n' {
		position--
	}

	return position
}

func seaTalkMarkdownPreviousLineStart(source []byte, lineStart int) int {
	if lineStart <= 0 {
		return -1
	}

	position := lineStart - 1
	for position > 0 && source[position-1] != '\n' {
		position--
	}

	return position
}

func seaTalkMarkdownLineEnd(source []byte, position int) int {
	if position < 0 {
		position = 0
	}

	for position < len(source) && source[position] != '\n' {
		position++
	}

	return position
}
