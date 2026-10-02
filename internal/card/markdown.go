package card

import (
	"fmt"
	"strings"
)

// Markdown — карточка для выгрузки (ФТ-39): собирается из структуры, а не
// из текста ответа, и с источниками (Р-10: card не знает про формат вывода
// сверх этого — новый вид выгрузки строится снаружи из тех же полей).
func Markdown(c Card) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n", c.Name)
	if c.Latin != "" {
		rank := c.RankRu
		if rank == "" {
			rank = strings.ToLower(c.Rank)
		}
		fmt.Fprintf(&b, "*%s*", c.Latin)
		if rank != "" {
			fmt.Fprintf(&b, " — %s", rank)
		}
		b.WriteString("\n\n")
	}
	if c.Summary != "" {
		b.WriteString(c.Summary + "\n\n")
	}
	if len(c.Tree) > 0 {
		b.WriteString("## Классификация\n\n")
		for _, n := range c.Tree {
			name := n.Name
			if n.NameRu != "" {
				name = n.NameRu + " (" + n.Name + ")"
			}
			rank := n.RankRu
			if rank == "" {
				rank = strings.ToLower(n.Rank)
			}
			fmt.Fprintf(&b, "- %s: %s\n", rank, name)
		}
		b.WriteByte('\n')
	}
	for _, s := range c.Sections {
		switch s.Status {
		case SectionRead:
			fmt.Fprintf(&b, "## %s\n\n%s\n\n", s.Title, s.Text)
			if s.Heading != "" {
				fmt.Fprintf(&b, "*Раздел статьи: «%s»*\n\n", s.Heading)
			}
		case SectionNone:
			fmt.Fprintf(&b, "## %s\n\nСведений нет: %s\n\n", s.Title, s.Reason)
		}
	}
	if len(c.Notes) > 0 {
		b.WriteString("## Оговорки\n\n")
		for _, n := range c.Notes {
			b.WriteString("- " + n + "\n")
		}
		b.WriteByte('\n')
	}
	writeSources(&b, c.Sources)
	return b.String()
}

func writeSources(b *strings.Builder, sources []Source) {
	if len(sources) == 0 {
		return
	}
	b.WriteString("## Источники\n\n")
	for _, s := range sources {
		fmt.Fprintf(b, "- [%s](%s)\n", s.Title, s.URL)
	}
	b.WriteByte('\n')
}

// ComparisonMarkdown — сравнение для выгрузки.
func ComparisonMarkdown(cmp Comparison) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s и %s\n\n", cmp.A.Name, cmp.B.Name)
	fmt.Fprintf(&b, "| | %s | %s |\n|---|---|---|\n", cmp.A.Name, cmp.B.Name)
	for _, r := range cmp.Rows {
		fmt.Fprintf(&b, "| %s | %s | %s |\n", r.Aspect, cellText(r.A), cellText(r.B))
	}
	b.WriteByte('\n')
	if len(cmp.Notes) > 0 {
		b.WriteString("## Оговорки\n\n")
		for _, n := range cmp.Notes {
			b.WriteString("- " + n + "\n")
		}
		b.WriteByte('\n')
	}
	writeSources(&b, []Source{
		{Title: "Википедия: " + cmp.A.Article, URL: cmp.A.ArticleURL},
		{Title: "Википедия: " + cmp.B.Article, URL: cmp.B.ArticleURL},
	})
	return b.String()
}

func cellText(c Cell) string {
	return strings.ReplaceAll(strings.ReplaceAll(c.Text, "|", "\\|"), "\n", " ")
}
