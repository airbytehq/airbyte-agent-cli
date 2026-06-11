package resources

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

func renderSkillDocs(docs map[string]any) string {
	metadata := mapValue(docs["metadata"])
	lines := []string{
		"# " + skillDocsTitle(docs, metadata),
		"Skill ID: " + stringValueWithDefault(metadata["id"], stringValue(docs["id"])),
		"Kind: " + stringValue(metadata["kind"]),
	}

	if summary := stringValue(metadata["summary"]); summary != "" {
		lines = append(lines, "Summary: "+summary)
	}

	if warnings := sliceValue(metadata["warnings"]); len(warnings) > 0 {
		lines = append(lines, "", "Warnings:")
		for _, warning := range warnings {
			lines = append(lines, "- "+stringValue(warning))
		}
	}

	sectionID := stringValue(docs["section_id"])
	content := sliceValue(docs["content"])
	if len(content) > 0 && sectionID == "" {
		for _, block := range content {
			lines = append(lines, renderSkillDocsBlock(block)...)
		}
	}

	outline := sliceValue(docs["outline"])
	if shouldRenderSkillDocsOutline(sectionID, content, outline) {
		lines = append(lines, "", "## Outline")
		for _, rawSection := range outline {
			section := mapValue(rawSection)
			outlineSectionID := stringValue(section["id"])
			title := stringValueWithDefault(section["title"], outlineSectionID)
			summary := stringValue(section["summary"])
			suffix := ""
			if summary != "" {
				suffix = " - " + summary
			}
			lines = append(lines, fmt.Sprintf("- `%s` %s%s", outlineSectionID, title, suffix))
		}
	}

	if sectionID != "" {
		lines = append(lines, "", fmt.Sprintf("## Section `%s`", sectionID))
		for _, block := range content {
			lines = append(lines, renderSkillDocsBlock(block)...)
		}
	}

	return strings.TrimSpace(strings.Join(lines, "\n"))
}

func skillDocsTitle(docs map[string]any, metadata map[string]any) string {
	return stringValueWithDefault(
		metadata["title"],
		stringValueWithDefault(metadata["id"], stringValueWithDefault(docs["id"], "Skill docs")),
	)
}

func shouldRenderSkillDocsOutline(sectionID string, content []any, outline []any) bool {
	if len(outline) == 0 {
		return false
	}
	// Mirror MCP's current rendering contract:
	// - section reads always show the outline for navigation context;
	// - outline-only docs need the generated outline;
	// - default docs with a backend-rendered Outline heading should not get
	//   a duplicate generated outline.
	return sectionID != "" || len(content) == 0 || !skillDocsContentHasOutline(content)
}

func skillDocsContentHasOutline(content []any) bool {
	for _, rawBlock := range content {
		block, ok := rawBlock.(map[string]any)
		if ok && block["type"] == "heading" && block["text"] == "Outline" {
			return true
		}
	}
	return false
}

func renderSkillDocsBlock(rawBlock any) []string {
	block, ok := rawBlock.(map[string]any)
	if !ok {
		return []string{"", compactJSON(rawBlock)}
	}

	switch block["type"] {
	case "heading":
		level, ok := headingLevel(block["level"])
		if !ok {
			return []string{"", compactJSON(block)}
		}
		return []string{"", strings.Repeat("#", level) + " " + stringValue(block["text"])}
	case "paragraph":
		return []string{"", stringValue(block["text"])}
	case "list":
		lines := []string{""}
		for _, item := range sliceValue(block["items"]) {
			lines = append(lines, "- "+stringValue(item))
		}
		return lines
	case "code":
		language := stringValue(block["language"])
		code := stringValue(block["code"])
		fence := codeFenceFor(code)
		return []string{"", fence + language, code, fence}
	case "table":
		return renderSkillDocsTable(block)
	default:
		return []string{"", compactJSON(block)}
	}
}

func renderSkillDocsTable(block map[string]any) []string {
	rawHeaders := sliceValue(block["headers"])
	if len(rawHeaders) == 0 {
		return []string{"", compactJSON(block)}
	}

	headers := make([]string, 0, len(rawHeaders))
	for _, header := range rawHeaders {
		headers = append(headers, escapeSkillDocsTableCell(header))
	}
	rows := sliceValue(block["rows"])
	maxColumns := len(headers)
	for _, rawRow := range rows {
		if rowLen := len(sliceValue(rawRow)); rowLen > maxColumns {
			maxColumns = rowLen
		}
	}
	for extra := len(headers) + 1; extra <= maxColumns; extra++ {
		headers = append(headers, fmt.Sprintf("Extra %d", extra-len(rawHeaders)))
	}

	rendered := []string{
		"",
		"| " + strings.Join(headers, " | ") + " |",
		"| " + strings.Join(repeatString("---", len(headers)), " | ") + " |",
	}

	for _, rawRow := range rows {
		row := sliceValue(rawRow)
		cells := make([]string, 0, len(headers))
		for i := 0; i < len(headers); i++ {
			if i < len(row) {
				cells = append(cells, escapeSkillDocsTableCell(row[i]))
			} else {
				cells = append(cells, "")
			}
		}
		rendered = append(rendered, "| "+strings.Join(cells, " | ")+" |")
	}

	return rendered
}

func codeFenceFor(code string) string {
	longestRun := 0
	currentRun := 0
	for _, r := range code {
		if r == '`' {
			currentRun++
			if currentRun > longestRun {
				longestRun = currentRun
			}
			continue
		}
		currentRun = 0
	}
	fenceLength := 3
	if longestRun >= fenceLength {
		fenceLength = longestRun + 1
	}
	return strings.Repeat("`", fenceLength)
}

func headingLevel(value any) (int, bool) {
	if value == nil {
		return 2, true
	}
	var level int
	switch n := value.(type) {
	case float64:
		level = int(n)
	case int:
		level = n
	case int64:
		level = int(n)
	case string:
		parsed, err := strconv.Atoi(n)
		if err != nil {
			return 0, false
		}
		level = parsed
	default:
		return 0, false
	}
	if level < 1 {
		level = 1
	}
	if level > 6 {
		level = 6
	}
	return level, true
}

func escapeSkillDocsTableCell(value any) string {
	out := stringValue(value)
	out = strings.ReplaceAll(out, "\\", "\\\\")
	out = strings.ReplaceAll(out, "|", "\\|")
	out = strings.ReplaceAll(out, "\r\n", "\n")
	out = strings.ReplaceAll(out, "\r", "\n")
	out = strings.ReplaceAll(out, "\n", " ")
	return out
}

func compactJSON(value any) string {
	raw, err := json.Marshal(value)
	if err != nil {
		return fmt.Sprint(value)
	}
	return string(raw)
}

func mapValue(value any) map[string]any {
	if m, ok := value.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

func sliceValue(value any) []any {
	switch v := value.(type) {
	case []any:
		return v
	case []string:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = item
		}
		return out
	default:
		return nil
	}
}

func stringValue(value any) string {
	if value == nil {
		return ""
	}
	if s, ok := value.(string); ok {
		return s
	}
	return fmt.Sprint(value)
}

func stringValueWithDefault(value any, fallback string) string {
	if s := stringValue(value); s != "" {
		return s
	}
	return fallback
}

func repeatString(value string, count int) []string {
	out := make([]string, count)
	for i := range out {
		out[i] = value
	}
	return out
}
