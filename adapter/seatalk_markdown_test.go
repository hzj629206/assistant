package adapter

import "testing"

func TestNormalizeSeaTalkMarkdownPreservesParagraphAndCodeFenceBlankLines(t *testing.T) {
	t.Parallel()

	text := "intro\n\n- item 1\n\n\tcontinued paragraph\n\n- item 2\n\n```go\n- code 1\n\n- code 2\n```\noutro"
	got := normalizeSeaTalkMarkdown(text)

	want := "intro\n\n- item 1\n\ncontinued paragraph\n\n- item 2\n\n```\n- code 1\n\n- code 2\n```\noutro"
	if got != want {
		t.Fatalf("unexpected normalized text: %q", got)
	}
}

func TestNormalizeSeaTalkMarkdownEscapesLooseTopLevelOrderedListMarkers(t *testing.T) {
	t.Parallel()

	text := "1. first item\n\n2. second item"
	got := normalizeSeaTalkMarkdown(text)

	want := "1\\. first item\n\n2\\. second item"
	if got != want {
		t.Fatalf("unexpected normalized text: %q", got)
	}
}

func TestNormalizeSeaTalkMarkdownEscapesSingleItemStrictTightTopLevelOrderedListMarker(t *testing.T) {
	t.Parallel()

	text := "1. only item"
	got := normalizeSeaTalkMarkdown(text)

	want := "1\\. only item"
	if got != want {
		t.Fatalf("unexpected normalized text: %q", got)
	}
}

func TestNormalizeSeaTalkMarkdownPreservesTightTopLevelOrderedListMarkers(t *testing.T) {
	t.Parallel()

	text := "1. first item\n2. second item"
	got := normalizeSeaTalkMarkdown(text)

	want := "1. first item\n2. second item"
	if got != want {
		t.Fatalf("unexpected normalized text: %q", got)
	}
}

func TestNormalizeSeaTalkMarkdownDoesNotAddBlankLinesBetweenLooseTopLevelUnorderedListItems(t *testing.T) {
	t.Parallel()

	text := "- item1\nContent\n- item2\n- item3"
	got := normalizeSeaTalkMarkdown(text)

	if got != text {
		t.Fatalf("unexpected normalized text: %q", got)
	}
}

func TestNormalizeSeaTalkMarkdownPreservesTightTopLevelUnorderedListSpacing(t *testing.T) {
	t.Parallel()

	text := "- item1\n- item2\n- item3"
	got := normalizeSeaTalkMarkdown(text)

	if got != text {
		t.Fatalf("unexpected normalized text: %q", got)
	}
}

func TestNormalizeSeaTalkMarkdownPromotesLooseTopLevelUnorderedListItemContinuationLines(t *testing.T) {
	t.Parallel()

	text := "- item1\n\n    continuation line\n\n- item2"
	got := normalizeSeaTalkMarkdown(text)

	want := "- item1\n\ncontinuation line\n\n- item2"
	if got != want {
		t.Fatalf("unexpected normalized text: %q", got)
	}
}

func TestNormalizeSeaTalkMarkdownPromotesLooseTopLevelUnorderedListItemBlockquoteAndCodeFence(t *testing.T) {
	t.Parallel()

	text := "- item1\n\n    > quote line\n\n    ```text\n    code line\n    ```\n\n- item2"
	got := normalizeSeaTalkMarkdown(text)

	want := "- item1\n\n> quote line\n\n```\ncode line\n```\n\n- item2"
	if got != want {
		t.Fatalf("unexpected normalized text: %q", got)
	}
}

func TestNormalizeSeaTalkMarkdownPromotesStrictGoldmarkTightTopLevelUnorderedListItemCodeFence(t *testing.T) {
	t.Parallel()

	//nolint:gosmopolitan // This test intentionally covers SeaTalk markdown normalization with Han text.
	text := "- 第一项：\n  ```text\n  example code\n  line 2\n  ```\n- 第二项\n- 第三项"
	got := normalizeSeaTalkMarkdown(text)

	//nolint:gosmopolitan // This test intentionally covers SeaTalk markdown normalization with Han text.
	want := "- 第一项：\n```\nexample code\nline 2\n```\n- 第二项\n- 第三项"
	if got != want {
		t.Fatalf("unexpected normalized text: %q", got)
	}
}

func TestNormalizeSeaTalkMarkdownPreservesNestedChildListInsideLooseTopLevelUnorderedListItem(t *testing.T) {
	t.Parallel()

	text := "- item1\n\n    - child item\n\n- item2"
	got := normalizeSeaTalkMarkdown(text)

	want := "- item1\n\n    - child item\n\n- item2"
	if got != want {
		t.Fatalf("unexpected normalized text: %q", got)
	}
}

func TestNormalizeSeaTalkMarkdownDoesNotInsertCrossBlockSpacingAfterLooseTopLevelUnorderedList(t *testing.T) {
	t.Parallel()

	text := "- item1\nContent\n\n```text\ncode line\n```\n\n> quoted line\n- item2\n- item3"
	got := normalizeSeaTalkMarkdown(text)

	want := "- item1\nContent\n\n```\ncode line\n```\n\n> quoted line\n- item2\n- item3"
	if got != want {
		t.Fatalf("unexpected normalized text: %q", got)
	}
}

func TestNormalizeSeaTalkMarkdownConvertsNestedListTwoSpaceIndentationToFourSpaces(t *testing.T) {
	t.Parallel()

	text := "- item 1\n  - item 1a\n  - item 1b\n- item 2"
	got := normalizeSeaTalkMarkdown(text)

	want := "- item 1\n    - item 1a\n    - item 1b\n- item 2"
	if got != want {
		t.Fatalf("unexpected normalized text: %q", got)
	}
}

func TestNormalizeSeaTalkMarkdownConvertsNestedListThreeSpaceIndentationToFourSpaces(t *testing.T) {
	t.Parallel()

	text := "- item 1\n   - item 1a\n   - item 1b\n- item 2"
	got := normalizeSeaTalkMarkdown(text)

	want := "- item 1\n    - item 1a\n    - item 1b\n- item 2"
	if got != want {
		t.Fatalf("unexpected normalized text: %q", got)
	}
}

func TestNormalizeSeaTalkMarkdownCompactsUnorderedListNestedInsideOrderedList(t *testing.T) {
	t.Parallel()

	text := "Topic:\n  1. parent item\n\n     - child item 1\n\n     - child item 2\n       continued line"
	got := normalizeSeaTalkMarkdown(text)

	want := "Topic:\n  1\\. parent item\n\n  - child item 1\n  - child item 2 continued line"
	if got != want {
		t.Fatalf("unexpected normalized text: %q", got)
	}
}

func TestNormalizeSeaTalkMarkdownPromotesLooseTopLevelOrderedListItemContinuationLines(t *testing.T) {
	t.Parallel()

	text := "1. parent item\n\n   - child item\n\n      * grandchild item\n\n2. another parent"
	got := normalizeSeaTalkMarkdown(text)

	want := "1\\. parent item\n\n- child item\n\n    - grandchild item\n\n2\\. another parent"
	if got != want {
		t.Fatalf("unexpected normalized text: %q", got)
	}
}

func TestNormalizeSeaTalkMarkdownPromotesLooseTopLevelOrderedListItemContinuationLinesWithTabs(t *testing.T) {
	t.Parallel()

	text := "1. parent item\n\n\t- child item\n\n\t\t* grandchild item\n\n2. another parent"
	got := normalizeSeaTalkMarkdown(text)

	want := "1\\. parent item\n\n- child item\n\n\t- grandchild item\n\n2\\. another parent"
	if got != want {
		t.Fatalf("unexpected normalized text: %q", got)
	}
}

func TestNormalizeSeaTalkMarkdownTreatsGoldmarkTightMixedOrderedListAsNonStrictTight(t *testing.T) {
	t.Parallel()

	//nolint:gosmopolitan // This test intentionally covers SeaTalk markdown normalization with Han text.
	text := "1.\t第一项\n\t这里有一些使用 tab 缩进的文本\n\t继续第一项的内容\n\t*\t嵌套无序列表项 A\n\t*\t嵌套无序列表项 B\n2.\t第二项\n\t第二项也有 tab 缩进\n\t和更多内容\n\t*\t嵌套无序列表项 C\n\t*\t嵌套无序列表项 D\n3.\t第三项\n\t简单的第三项\n\t*\t嵌套无序列表项 E\n"
	got := normalizeSeaTalkMarkdown(text)

	//nolint:gosmopolitan // This test intentionally covers SeaTalk markdown normalization with Han text.
	want := "1\\. 第一项\n这里有一些使用 tab 缩进的文本\n继续第一项的内容\n- 嵌套无序列表项 A\n- 嵌套无序列表项 B\n2\\. 第二项\n第二项也有 tab 缩进\n和更多内容\n- 嵌套无序列表项 C\n- 嵌套无序列表项 D\n3\\. 第三项\n简单的第三项\n- 嵌套无序列表项 E\n"
	if got != want {
		t.Fatalf("unexpected normalized text: %q", got)
	}
}

func TestNormalizeSeaTalkMarkdownCompactsNestedOrderedListSpacing(t *testing.T) {
	t.Parallel()

	text := "- parent item\n  1. child item 1\n\n  2. child item 2"
	got := normalizeSeaTalkMarkdown(text)

	want := "- parent item\n    1. child item 1\n    2. child item 2"
	if got != want {
		t.Fatalf("unexpected normalized text: %q", got)
	}
}

func TestNormalizeSeaTalkMarkdownCompactsNestedUnorderedListSpacingWithoutOrderedAncestor(t *testing.T) {
	t.Parallel()

	text := "- parent item\n  - child item 1\n\n  - child item 2\n    continued line"
	got := normalizeSeaTalkMarkdown(text)

	want := "- parent item\n    - child item 1\n    - child item 2 continued line"
	if got != want {
		t.Fatalf("unexpected normalized text: %q", got)
	}
}

func TestNormalizeSeaTalkMarkdownCompactsListMarkerSpacingToSingleSpace(t *testing.T) {
	t.Parallel()

	//nolint:gosmopolitan // This test intentionally covers SeaTalk markdown normalization with Han text.
	text := "*   第一层无序列表项 1\n    *   第二层无序列表项 1.1\n        1.  第三层有序列表项 1.1.1\n        2.  第三层有序列表项 1.1.2\n            *   第四层无序列表项 1.1.2.1\n            *   第四层无序列表项 1.1.2.2"
	got := normalizeSeaTalkMarkdown(text)

	//nolint:gosmopolitan // This test intentionally covers SeaTalk markdown normalization with Han text.
	want := "- 第一层无序列表项 1\n    - 第二层无序列表项 1.1\n        1. 第三层有序列表项 1.1.1\n        2. 第三层有序列表项 1.1.2\n            - 第四层无序列表项 1.1.2.1\n            - 第四层无序列表项 1.1.2.2"
	if got != want {
		t.Fatalf("unexpected normalized text: %q", got)
	}
}

func TestNormalizeSeaTalkMarkdownNormalizesNestedIndentationRelativeToParent(t *testing.T) {
	t.Parallel()

	//nolint:gosmopolitan // This test intentionally covers SeaTalk markdown normalization with Han text.
	text := "**纯无序列表嵌套**\n* 第一层\n  * 第二层\n    * 第三层\n  * 第二层另一个\n* 第一层另一个"
	got := normalizeSeaTalkMarkdown(text)

	//nolint:gosmopolitan // This test intentionally covers SeaTalk markdown normalization with Han text.
	want := "**纯无序列表嵌套**\n- 第一层\n    - 第二层\n        - 第三层\n    - 第二层另一个\n- 第一层另一个"
	if got != want {
		t.Fatalf("unexpected normalized text: %q", got)
	}
}

func TestNormalizeSeaTalkMarkdownPreservesPlusUnorderedListMarkers(t *testing.T) {
	t.Parallel()

	text := "+ parent item\n  + child item\n    + grandchild item"
	got := normalizeSeaTalkMarkdown(text)

	want := "+ parent item\n    + child item\n        + grandchild item"
	if got != want {
		t.Fatalf("unexpected normalized text: %q", got)
	}
}

func TestNormalizeSeaTalkMarkdownPreservesLineBreakAfterEmphasizedHeadingLine(t *testing.T) {
	t.Parallel()

	//nolint:gosmopolitan // This test intentionally covers SeaTalk markdown normalization with Han text.
	text := "*1. 2026-04-16 产品大更新*\nCodex 从“写代码代理”扩展成更广义的工作空间：新增 *in-app browser*、*computer use*（可操作 macOS 应用）、*artifact viewer*、*chat/automation* 连续线程、PR 评审侧边栏、记忆能力、SSH 远程连接 alpha、多终端、多窗口、新插件。官方单独发布了《Codex for (almost) everything》。"
	got := normalizeSeaTalkMarkdown(text)

	//nolint:gosmopolitan // This test intentionally covers SeaTalk markdown normalization with Han text.
	want := "*1. 2026-04-16 产品大更新*\nCodex 从“写代码代理”扩展成更广义的工作空间：新增 **in-app browser**、**computer use**（可操作 macOS 应用）、**artifact viewer**、**chat/automation** 连续线程、PR 评审侧边栏、记忆能力、SSH 远程连接 alpha、多终端、多窗口、新插件。官方单独发布了《Codex for (almost) everything》。"
	if got != want {
		t.Fatalf("unexpected normalized text: %q", got)
	}
}

func TestNormalizeSeaTalkMarkdownConvertsItalicToBold(t *testing.T) {
	t.Parallel()

	//nolint:gosmopolitan // This test intentionally covers Han text emphasis normalization.
	text := "这是在测试*斜体*内容。"
	got := normalizeSeaTalkMarkdown(text)

	//nolint:gosmopolitan // This test intentionally covers Han text emphasis normalization.
	want := "这是在测试**斜体**内容。"
	if got != want {
		t.Fatalf("unexpected normalized text: %q", got)
	}
}

func TestNormalizeSeaTalkMarkdownConvertsItalicToBoldWithExistingLeadingSpace(t *testing.T) {
	t.Parallel()

	//nolint:gosmopolitan // This test intentionally covers Han text emphasis normalization.
	text := "这是在测试 *斜体*内容。"
	got := normalizeSeaTalkMarkdown(text)

	//nolint:gosmopolitan // This test intentionally covers Han text emphasis normalization.
	want := "这是在测试 **斜体**内容。"
	if got != want {
		t.Fatalf("unexpected normalized text: %q", got)
	}
}

func TestNormalizeSeaTalkMarkdownConvertsItalicToBoldWithExistingTrailingSpace(t *testing.T) {
	t.Parallel()

	//nolint:gosmopolitan // This test intentionally covers Han text emphasis normalization.
	text := "这是在测试*斜体* 内容。"
	got := normalizeSeaTalkMarkdown(text)

	//nolint:gosmopolitan // This test intentionally covers Han text emphasis normalization.
	want := "这是在测试**斜体** 内容。"
	if got != want {
		t.Fatalf("unexpected normalized text: %q", got)
	}
}

func TestNormalizeSeaTalkMarkdownPreservesItalicWithWhitespaceOnBothSides(t *testing.T) {
	t.Parallel()

	//nolint:gosmopolitan // This test intentionally covers Han text emphasis normalization.
	text := "这是在测试 *斜体* 内容。"
	got := normalizeSeaTalkMarkdown(text)

	if got != text {
		t.Fatalf("unexpected normalized text: %q", got)
	}
}

func TestNormalizeSeaTalkMarkdownPreservesItalicBeforeComma(t *testing.T) {
	t.Parallel()

	text := "Punctuation test: *italic before comma*, next."
	got := normalizeSeaTalkMarkdown(text)

	if got != text {
		t.Fatalf("unexpected normalized text: %q", got)
	}
}

func TestNormalizeSeaTalkMarkdownPreservesItalicBeforePeriod(t *testing.T) {
	t.Parallel()

	text := "Punctuation test: *italic before period*."
	got := normalizeSeaTalkMarkdown(text)

	if got != text {
		t.Fatalf("unexpected normalized text: %q", got)
	}
}

func TestNormalizeSeaTalkMarkdownPreservesItalicInsideParentheses(t *testing.T) {
	t.Parallel()

	text := "Parentheses test: (*this part is italic inside parentheses*)."
	got := normalizeSeaTalkMarkdown(text)

	if got != text {
		t.Fatalf("unexpected normalized text: %q", got)
	}
}

func TestNormalizeSeaTalkMarkdownSplitsItalicAroundNestedInlineCode(t *testing.T) {
	t.Parallel()

	text := "*this is `nested inline code` in italic text*"
	got := normalizeSeaTalkMarkdown(text)

	want := "*this is* `nested inline code` *in italic text*"
	if got != want {
		t.Fatalf("unexpected normalized text: %q", got)
	}
}

func TestNormalizeSeaTalkMarkdownSplitsTightItalicAroundNestedInlineCodeAndConvertsToBold(t *testing.T) {
	t.Parallel()

	//nolint:gosmopolitan // This test intentionally covers Han text emphasis normalization.
	text := "这是在测试*斜体 `inline code` 内容*。"
	got := normalizeSeaTalkMarkdown(text)

	//nolint:gosmopolitan // This test intentionally covers Han text emphasis normalization.
	want := "这是在测试**斜体** `inline code` **内容**。"
	if got != want {
		t.Fatalf("unexpected normalized text: %q", got)
	}
}

func TestNormalizeSeaTalkMarkdownSplitsBoldAroundNestedInlineCode(t *testing.T) {
	t.Parallel()

	text := "**this is `nested inline code` in bold text**"
	got := normalizeSeaTalkMarkdown(text)

	want := "**this is** `nested inline code` **in bold text**"
	if got != want {
		t.Fatalf("unexpected normalized text: %q", got)
	}
}

func TestNormalizeSeaTalkMarkdownPreservesStandaloneBoldItalic(t *testing.T) {
	t.Parallel()

	text := "***content***"
	got := normalizeSeaTalkMarkdown(text)

	if got != text {
		t.Fatalf("unexpected normalized text: %q", got)
	}
}

func TestNormalizeSeaTalkMarkdownSplitsBoldItalicAroundNestedInlineCode(t *testing.T) {
	t.Parallel()

	text := "***content `code` more***"
	got := normalizeSeaTalkMarkdown(text)

	want := "***content*** `code` ***more***"
	if got != want {
		t.Fatalf("unexpected normalized text: %q", got)
	}
}

func TestNormalizeSeaTalkMarkdownAddsSpacesWhenSplittingBoldAroundNestedItalic(t *testing.T) {
	t.Parallel()

	//nolint:gosmopolitan // This test intentionally covers Han text emphasis normalization.
	text := "**这是带*斜体*的粗体**"
	got := normalizeSeaTalkMarkdown(text)

	//nolint:gosmopolitan // This test intentionally covers Han text emphasis normalization.
	want := "**这是带** *斜体* **的粗体**"
	if got != want {
		t.Fatalf("unexpected normalized text: %q", got)
	}
}

func TestNormalizeSeaTalkMarkdownSplitsBoldAroundTrailingNestedItalic(t *testing.T) {
	t.Parallel()

	//nolint:gosmopolitan // This test intentionally covers Han text emphasis normalization.
	text := "**粗体里放 *斜体***"
	got := normalizeSeaTalkMarkdown(text)

	//nolint:gosmopolitan // This test intentionally covers Han text emphasis normalization.
	want := "**粗体里放** *斜体*"
	if got != want {
		t.Fatalf("unexpected normalized text: %q", got)
	}
}

func TestNormalizeSeaTalkMarkdownSplitsItalicAroundNestedBold(t *testing.T) {
	t.Parallel()

	//nolint:gosmopolitan // This test intentionally covers Han text emphasis normalization.
	text := "这是*带**粗体**的斜体*"
	got := normalizeSeaTalkMarkdown(text)

	//nolint:gosmopolitan // This test intentionally covers Han text emphasis normalization.
	want := "这是**带** **粗体** *的斜体*"
	if got != want {
		t.Fatalf("unexpected normalized text: %q", got)
	}
}

func TestNormalizeSeaTalkMarkdownSplitsHanBoldItalicAroundNestedInlineCode(t *testing.T) {
	t.Parallel()

	//nolint:gosmopolitan // This test intentionally covers Han text bold-italic normalization.
	text := "这是***粗斜体 `code` 内容***。"
	got := normalizeSeaTalkMarkdown(text)

	//nolint:gosmopolitan // This test intentionally covers Han text bold-italic normalization.
	want := "这是***粗斜体*** `code` ***内容***。"
	if got != want {
		t.Fatalf("unexpected normalized text: %q", got)
	}
}

func TestNormalizeSeaTalkMarkdownAddsSpacesWhenSplittingHanBoldAroundNestedInlineCode(t *testing.T) {
	t.Parallel()

	//nolint:gosmopolitan // This test intentionally covers Han text bold normalization.
	text := "**这是带`code`的粗体**"
	got := normalizeSeaTalkMarkdown(text)

	//nolint:gosmopolitan // This test intentionally covers Han text bold normalization.
	want := "**这是带** `code` **的粗体**"
	if got != want {
		t.Fatalf("unexpected normalized text: %q", got)
	}
}

func TestNormalizeSeaTalkMarkdownPreservesTightBoldItalicBoundaryBehavior(t *testing.T) {
	t.Parallel()

	//nolint:gosmopolitan // This test intentionally covers Han text bold-italic preservation.
	text := "这是***粗斜体***内容。"
	got := normalizeSeaTalkMarkdown(text)

	//nolint:gosmopolitan // This test intentionally covers Han text bold-italic preservation.
	want := "这是***粗斜体***内容。"
	if got != want {
		t.Fatalf("unexpected normalized text: %q", got)
	}
}

func TestNormalizeSeaTalkMarkdownPreservesExistingBoldText(t *testing.T) {
	t.Parallel()

	//nolint:gosmopolitan // This test intentionally covers Han text emphasis preservation.
	text := "这是在测试 **粗体** 内容。"
	got := normalizeSeaTalkMarkdown(text)

	if got != text {
		t.Fatalf("unexpected normalized text: %q", got)
	}
}
